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
  ['srcdoc', /\bsrcdoc\s*=/],
  ['DOMParser', /\bDOMParser\b/],
  ['setHTMLUnsafe', /\b(setHTMLUnsafe|parseHTMLUnsafe)\b/],
  ['eval', /\beval\s*\(/],
  ['new Function', /\bnew\s+Function\s*\(/],
  ['string timer', /\bset(Timeout|Interval)\s*\(\s*['"`]/],
];

test('app.js exists and is checked', () => {
  assert.ok(scripts.includes('app.js'));
  assert.ok(scripts.includes('lib.js'));
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
    assert.doesNotMatch(src, /\blocalStorage\b|\bindexedDB\b|document\s*\.\s*cookie/, `${file}: the API key belongs in sessionStorage only`);
    // Absolute URLs may appear only as form placeholders.
    const withoutPlaceholders = src.replace(/placeholder:\s*'[^']*'/g, '');
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
  const src = code(read('app.js'));
  assert.match(src, /sessionStorage\.setItem\(KEY_STORAGE, key\)/);
});

test('links built by app.js only go to in-app routes', () => {
  const src = code(read('app.js'));
  // Every href goes through safeHref, which accepts "#/…" only, except the
  // download link for an attachment blob created in downloadAttachment.
  const hrefs = [...src.matchAll(/\.href\s*=\s*([^;]+);/g)].map((m) => m[1].trim());
  assert.deepEqual(hrefs, ['url'], `unexpected href assignments: ${hrefs.join(', ')}`);
  assert.match(src, /new Blob\(\[[^\]]+\], \{ type: 'application\/octet-stream' \}\)/, 'attachments must download as octet-stream, never render');
  assert.match(src, /function safeHref\(v\) \{\s*const s = String\(v\);\s*if \(s\.startsWith\('#\/'\)\) return s;\s*throw/);
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
