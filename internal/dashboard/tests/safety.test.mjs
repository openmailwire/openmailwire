// Static checks on the dashboard's files. The page holds an API key and shows
// attacker-controlled data (subjects, addresses, SMTP replies, webhook URLs,
// inbound mail), so API data must never be parsed as HTML.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const staticDir = join(dirname(fileURLToPath(import.meta.url)), '..', 'static');
const read = (name) => readFileSync(join(staticDir, name), 'utf8');
const scripts = readdirSync(staticDir).filter((f) => f.endsWith('.js'));

// Strips comments so that a comment explaining a rule does not trip it.
function code(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:\\])\/\/.*$/gm, '$1');
}

const HTML_SINKS = [
  ['innerHTML', /\binnerHTML\b/],
  ['outerHTML', /\bouterHTML\b/],
  ['insertAdjacentHTML', /\binsertAdjacentHTML\b/],
  ['document.write', /\bdocument\s*\.\s*write(ln)?\b/],
  ['createContextualFragment', /\bcreateContextualFragment\b/],
  ['DOMParser', /\bDOMParser\b/],
  ['setHTMLUnsafe', /\b(setHTMLUnsafe|parseHTMLUnsafe)\b/],
  ['eval', /\beval\s*\(/],
  ['new Function', /\bnew\s+Function\s*\(/],
  ['string timer', /\bset(Timeout|Interval)\s*\(\s*['"`]/],
];

test('every script exists and is checked', () => {
  for (const f of ['app.js', 'lib.js', 'ui.js', 'ops.js', 'domains.js', 'home.js', 'compose.js', 'mail.js']) assert.ok(scripts.includes(f), f);
});

test('every script is reachable from app.js, and nothing else is loaded', () => {
  const seen = new Set(['app.js']);
  const queue = ['app.js'];
  while (queue.length) {
    const src = code(read(queue.shift()));
    for (const m of src.matchAll(/^\s*import\b[^'"]*['"]\.\/([\w-]+\.js)['"]/gm)) {
      if (!seen.has(m[1])) {
        seen.add(m[1]);
        queue.push(m[1]);
      }
    }
  }
  assert.deepEqual([...seen].sort(), [...scripts].sort(), 'a script in static/ is not imported, or an import is missing');
});

for (const file of scripts) {
  test(`${file} has no HTML sinks`, () => {
    // The raw text, comments included: the rule is simply "never there".
    const src = read(file);
    for (const [name, re] of HTML_SINKS) {
      assert.doesNotMatch(src, re, `${file} uses ${name}; build DOM nodes with createElement/textContent instead`);
    }
  });

  test(`${file} keeps the key out of persistent storage and talks to no one else`, () => {
    const src = code(read(file));
    // localStorage is allowed only in ui.js getPref/setPref (non-secret
    // settings), checked below.
    const withoutPrefs = file === 'ui.js' ? src.replace(/export function (getPref|setPref)\(name(, value)?\) \{[\s\S]*?\n\}/g, '') : src;
    assert.doesNotMatch(withoutPrefs, /\blocalStorage\b|\bindexedDB\b|document\s*\.\s*cookie/, `${file}: the API key belongs in sessionStorage only`);
    // Absolute URLs may appear only as form placeholders, and as the SVG
    // namespace name (which is never fetched).
    const withoutPlaceholders = src.replace(/placeholder:\s*'[^']*'/g, '').replace(/'http:\/\/www\.w3\.org\/2000\/svg'/g, '');
    assert.doesNotMatch(withoutPlaceholders, /https?:\/\//, `${file}: no absolute URLs; the dashboard only calls its own origin`);
    assert.doesNotMatch(src, /XMLHttpRequest|WebSocket|EventSource|sendBeacon|importScripts|new\s+Worker\b/, `${file}: requests go through fetch to the API only`);
    for (const m of src.matchAll(/\bfetch\(\s*([^,)]+)/g)) {
      assert.match(m[1].trim(), /^(API \+|`\$\{API\})/, `${file}: fetch(${m[1]}) must call the same-origin API`);
    }
    assert.doesNotMatch(src, /\bimport\s*\(/, `${file}: no dynamic imports`);
    for (const m of src.matchAll(/^\s*import\b[^'"]*['"]([^'"]+)['"]/gm)) {
      assert.match(m[1], /^\.\/[\w-]+\.js$/, `${file} imports ${m[1]}; only local modules are allowed`);
    }
  });

  test(`${file} sets no inline styles or event-handler attributes`, () => {
    const src = code(read(file));
    assert.doesNotMatch(src, /setAttribute\(\s*['"](style|on\w+)['"]/, `${file}: inline styles and handlers break the CSP`);
    assert.doesNotMatch(src, /\.style\s*\.\s*\w+\s*=|\.style\s*=|cssText/, `${file}: use classes, not inline styles`);
  });
}

test('the API key is only ever stored in sessionStorage', () => {
  const src = code(read('ui.js'));
  assert.match(src, /sessionStorage\.setItem\(KEY_STORAGE, key\)/);
  for (const file of scripts) {
    assert.doesNotMatch(code(read(file)), /\bsessionStorage\.setItem\((?!KEY_STORAGE, key\))/, `${file} stores something else`);
    assert.doesNotMatch(code(read(file)), /localStorage\.setItem\((?!PREF_PREFIX \+ name, String\(value\)\))/, `${file} stores something else`);
    assert.doesNotMatch(code(read(file)), /localStorage[^;]*KEY_STORAGE|KEY_STORAGE[^;]*localStorage/, `${file} puts the key in localStorage`);
  }
});

test('links built by the scripts only go to in-app routes', () => {
  // Every href goes through safeHref, which accepts "#/…" only, except the
  // download link for a blob created in download() (ui.js).
  const hrefs = scripts.flatMap((file) => [...code(read(file)).matchAll(/\.href\s*=\s*([^;]+);/g)].map((m) => `${file}: ${m[1].trim()}`));
  assert.deepEqual(hrefs, ['ui.js: url'], `unexpected href assignments: ${hrefs.join(', ')}`);
  for (const file of scripts) {
    assert.doesNotMatch(code(read(file)), /setAttribute\(\s*['"](href|src|action|formaction|xlink:href)['"]\s*,(?!\s*safeHref\()/, `${file} sets a link attribute without safeHref`);
    assert.doesNotMatch(code(read(file)), /\.(src|action|formAction|data)\s*=(?!=)/, `${file} assigns a URL property`);
  }
  const ui = code(read('ui.js'));
  assert.match(ui, /new Blob\(\[[^\]]+\], \{ type: 'application\/octet-stream' \}\)/, 'downloads must be octet-stream, never rendered');
  assert.match(ui, /function safeHref\(v\) \{\s*const s = String\(v\);\s*if \(s\.startsWith\('#\/'\)\) return s;\s*throw/);
  // Pages build elements with h() and svg() from ui.js only.
  for (const file of scripts.filter((f) => f !== 'ui.js')) {
    assert.doesNotMatch(code(read(file)), /document\.createElement(NS)?\(/, `${file}: build elements with h() or svg() from ui.js`);
  }
});

test('the SVG helper only allows shapes and geometry', () => {
  const ui = code(read('ui.js'));
  const tags = ui.match(/const SVG_TAGS = new Set\(\[([^\]]*)\]\)/);
  const attrs = ui.match(/const SVG_ATTRS = new Set\(\[([^\]]*)\]\)/);
  assert.ok(tags && attrs, 'SVG_TAGS and SVG_ATTRS must be literal sets');
  const list = (m) => [...m[1].matchAll(/'([^']+)'/g)].map((x) => x[1]);
  for (const t of list(tags)) assert.match(t, /^(svg|rect|line|g|path|circle)$/, `svg tag ${t}`);
  for (const a of list(attrs)) {
    assert.doesNotMatch(a, /^(on|href|xlink|style|src)/i, `svg attribute ${a}`);
  }
});

test('received mail is marked as sender-provided where it is shown', () => {
  const app = code(read('app.js'));
  // The message page and the inbox list mark sender-written text.
  assert.match(app, /senderTag\(\)/);
  assert.match(app, /untrusted\(mono\(m\.from/);
  assert.match(app, /pageHead\(untrusted\(m\.subject/);
  assert.match(app, /class: 'body untrusted-box' \}, m\.text\)/);
  // HTML mail is only ever shown as source text.
  assert.doesNotMatch(app, /m\.html\)\s*[,;]\s*$/m);
});

test('index.html has no inline code and loads only local files', () => {
  const html = read('index.html');
  for (const tag of html.match(/<script\b[^>]*>/gi) || []) {
    assert.match(tag, /\ssrc="[\w.-]+\.js"/, `inline or remote script: ${tag}`);
  }
  assert.doesNotMatch(html, /<style\b/i, 'inline <style>');
  assert.doesNotMatch(html, /\sstyle\s*=/i, 'inline style attribute');
  assert.doesNotMatch(html, /\son[a-z]+\s*=/i, 'inline event handler');
  assert.doesNotMatch(html, /javascript:/i);
  for (const tag of html.match(/<(link|img|source|script)\b[^>]*>/gi) || []) {
    assert.doesNotMatch(tag, /(src|href|srcset)="(https?:)?\/\//i, `remote resource: ${tag}`);
  }
});

test('styles.css loads nothing remote', () => {
  const css = read('styles.css');
  assert.doesNotMatch(css, /@import/i);
  assert.doesNotMatch(css, /url\(\s*['"]?(https?:)?\/\//i);
});

test('localStorage holds only prefixed, non-secret preferences', () => {
  const ui = code(read('ui.js'));
  const uses = [...ui.matchAll(/localStorage\.(\w+)\(((?:[^()]|\([^()]*\))*)\)/g)].map((m) => `${m[1]}(${m[2]})`);
  assert.deepEqual(uses.sort(), [
    'getItem(PREF_PREFIX + name)', 'removeItem(PREF_PREFIX + name)', 'setItem(PREF_PREFIX + name, String(value))',
  ]);
  assert.match(ui, /const PREF_PREFIX = 'openmailwire\.pref\.';/);
  // Callers pass literal names, and never the key or a token.
  for (const file of scripts) {
    for (const m of code(read(file)).matchAll(/\b(getPref|setPref)\(([^,)]*)/g)) {
      if (m[2] === 'name') continue; // the definitions
      assert.match(m[2].trim(), /^(SEEN_PREF|'[a-z.]+')$/, `${file}: ${m[0]} must name a fixed preference`);
    }
  }
});

test('HTML is only ever shown in a fully sandboxed frame, and only the compose preview uses it', () => {
  const sets = scripts.flatMap((file) => [...code(read(file)).matchAll(/\.srcdoc\s*=/g)].map(() => file));
  assert.deepEqual(sets, ['ui.js'], 'srcdoc may only be set in ui.js setPreview');
  const ui = code(read('ui.js'));
  assert.match(ui, /export function setPreview\(frame, html\) \{\s*if \(!frame \|\| frame\.tagName !== 'IFRAME' \|\| frame\.getAttribute\('sandbox'\) !== ''\) \{\s*throw/);
  assert.match(ui, /h\('iframe', \{ class: 'preview-frame', sandbox: true,/, 'the preview frame is sandboxed with no permissions');
  assert.doesNotMatch(ui, /allow-scripts|allow-same-origin|allow-popups|allow-forms|allow-top-navigation/);
  // h() itself refuses srcdoc, so no other code path can set it.
  assert.match(ui, /k === 'style' \|\| k === 'srcdoc'\) throw/);
  const callers = scripts.filter((file) => file !== 'ui.js' && /\bsetPreview\(/.test(code(read(file))));
  assert.deepEqual(callers, ['compose.js'], 'only Compose previews HTML; received mail is shown as source text');
  // Compose previews what the person typed, never API data.
  for (const m of code(read('compose.js')).matchAll(/setPreview\(([^)]*)\)/g)) {
    assert.equal(m[1].trim(), 'frame, html.value');
  }
});

test('the Cloudflare token never reaches storage or a URL', () => {
  const src = code(read('domains.js'));
  // The field is a password field that browsers do not fill or remember.
  assert.match(src, /type: 'password', name: 'cloudflare-token', autocomplete: 'off'/);
  // The token is read in one place, into the request body only.
  const reads = [...src.matchAll(/tokenInput\.value/g)].length;
  assert.equal(reads, 2, 'tokenInput.value: one read and one clear');
  assert.match(src, /const apiToken = tokenInput\.value\.trim\(\);/);
  assert.match(src, /const forget = \(\) => \{ tokenInput\.value = ''; \};/);
  const uses = [...src.matchAll(/\bapiToken\b/g)].length;
  assert.equal(uses, 3, 'apiToken: declared, checked, and put in the body');
  assert.match(src, /req\.api_token = apiToken;/);
  assert.match(src, /return ctx\.api\('POST', path, req\);/);
  assert.doesNotMatch(src, /listPath\([^)]*req|route\([^)]*req|location[^;]*req/);
  // It is cleared after Apply (success or failure) and on Cancel.
  assert.ok((src.match(/\bforget\(\);/g) || []).length >= 3, 'the token field is cleared after use');
  for (const file of scripts) {
    assert.doesNotMatch(code(read(file)), /(setPref|setItem)\([^)]*(token|apiToken|api_token)/i, `${file} stores a token`);
  }
});
