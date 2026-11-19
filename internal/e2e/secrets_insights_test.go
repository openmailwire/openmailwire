package e2e

// v0.6 lane E: the start-up key check, secret rotation and GET /insights.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

func legacySealed(t *testing.T, key, plain []byte) []byte {
	t.Helper()
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, nil)
}

func wantKeyCheckError(t *testing.T, err error, contains string) {
	t.Helper()
	var kc *store.KeyCheckError
	if !errors.As(err, &kc) || !strings.Contains(kc.Msg, contains) {
		t.Fatalf("want a refusal mentioning %q, got %v", contains, err)
	}
}

// R4 (informational): the control plane checks OMW_SECRET_KEY(S) against
// the database at start; and OMW_SECRET_KEYS rotation end to end.
func TestSecretKeyCheckAndRotation(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.st.Pool.Exec(e.ctx, `TRUNCATE secret_keys, secret_values`); err != nil {
		t.Fatal(err)
	}
	A := e.secretKey // the key the env's API seals with
	B := bytes.Repeat([]byte{'b'}, 32)
	C := bytes.Repeat([]byte{'c'}, 32)

	// Data as an upgraded v0.5 database has it: a DKIM key sealed by this
	// version and a webhook secret in the legacy format.
	e.addVerifiedDomain("acme.test")
	code, hook := e.call("POST", "/webhooks", "", map[string]any{"endpoint": "https://hooks.example.com/x"})
	if code != 201 {
		t.Fatalf("webhook: %d %v", code, hook)
	}
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE webhooks SET secret = $1 WHERE id = $2`,
		legacySealed(t, A, []byte("whsec_bGVnYWN5")), hook["id"]); err != nil {
		t.Fatal(err)
	}

	// The wrong key is refused before anything is registered.
	_, err := e.st.VerifySecretKeys(e.ctx, [][]byte{B})
	wantKeyCheckError(t, err, "does not open the data already in this database")

	root, err := e.st.VerifySecretKeys(e.ctx, [][]byte{A})
	if err != nil || !bytes.Equal(root, A) {
		t.Fatalf("first start with the right key: %v", err)
	}
	if _, err := e.st.VerifySecretKeys(e.ctx, [][]byte{A}); err != nil {
		t.Fatalf("second start: %v", err)
	}
	// A new key alone: the data is still sealed with A.
	_, err = e.st.VerifySecretKeys(e.ctx, [][]byte{B})
	wantKeyCheckError(t, err, secret.KeyID(A))

	// Rotation: the new key first, the old one after it.
	root, err = e.st.VerifySecretKeys(e.ctx, [][]byte{B, A})
	if err != nil || !bytes.Equal(root, A) {
		t.Fatalf("start with B,A: root changed or %v", err)
	}
	ring, _ := secret.NewKeyring([][]byte{B, A})
	for _, c := range store.SealedColumns {
		if _, err := e.st.ResealColumn(e.ctx, ring, c, store.ResealOptions{Batch: 1}); err != nil {
			t.Fatal(err)
		}
		if n, err := e.st.CountUnsealedByPrimary(e.ctx, ring, c, 10); err != nil || n != 0 {
			t.Fatalf("%s: %d values not resealed (%v)", c, n, err)
		}
	}
	retired, err := e.st.RetireSecretKeys(e.ctx, ring.PrimaryID())
	if err != nil || len(retired) != 1 || retired[0] != secret.KeyID(A) {
		t.Fatalf("retired %v %v", retired, err)
	}

	// Now B alone opens everything, and the API keeps working with it.
	onlyB, _ := secret.NewSealer(B)
	root, err = e.st.VerifySecretKeys(e.ctx, [][]byte{B})
	if err != nil || !bytes.Equal(root, A) {
		t.Fatalf("start with B after rotation: %v (the bounce-address root must stay A)", err)
	}
	var whsec []byte
	e.st.Pool.QueryRow(e.ctx, `SELECT secret FROM webhooks WHERE id = $1`, hook["id"]).Scan(&whsec)
	if plain, err := onlyB.Open(whsec); err != nil || string(plain) != "whsec_bGVnYWN5" {
		t.Fatalf("legacy webhook secret after rotation: %q %v", plain, err)
	}
	e.srv.Sealer = onlyB
	e.send("x@example.test") // opens the DKIM key with B
	_, err = e.st.VerifySecretKeys(e.ctx, [][]byte{C})
	wantKeyCheckError(t, err, secret.KeyID(B))

	// A key whose check value does not match the stored one.
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE secret_keys SET check_value = '\x00' WHERE key_id = $1`, secret.KeyID(B)); err != nil {
		t.Fatal(err)
	}
	_, err = e.st.VerifySecretKeys(e.ctx, [][]byte{B})
	wantKeyCheckError(t, err, "is not the key this database registered")
	if _, err := e.st.Pool.Exec(e.ctx, `TRUNCATE secret_keys, secret_values`); err != nil {
		t.Fatal(err)
	}
}

func insightNear(v any, want float64) bool {
	f, ok := v.(float64)
	return ok && math.Abs(f-want) < 1e-6
}

func TestInsights(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	ids := []string{e.send("a@gmail.test"), e.send("b@gmail.test"), e.send("c@outlook.test"), e.send("d@other.test")}
	attempt := func(email, mx, ip, outcome string, after int) {
		t.Helper()
		if _, err := e.st.Pool.Exec(e.ctx, `INSERT INTO attempts (id, delivery_id, source_ip, mx_host, started_at, finished_at, outcome)
			SELECT gen_random_uuid(), d.id, $2, $3, m.created_at + make_interval(secs => $5),
				m.created_at + make_interval(secs => $5), $4
			FROM deliveries d JOIN emails m ON m.id = d.email_id WHERE d.email_id = $1`, email, ip, mx, outcome, after); err != nil {
			t.Fatal(err)
		}
	}
	attempt(ids[0], "aspmx.l.google.com.", "203.0.113.7", "deferred", 10)
	attempt(ids[0], "alt1.aspmx.l.google.com", "203.0.113.7", "delivered", 60)
	attempt(ids[1], "aspmx.l.google.com", "203.0.113.7", "delivered", 120)
	attempt(ids[2], "acme-test.mail.protection.outlook.com", "203.0.113.8", "bounced", 5)
	attempt(ids[3], "mx.other.test", "203.0.113.8", "delivered", 30)
	// The last email was sent ten days ago: only the 30-day window has it.
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE emails SET created_at = created_at - interval '10 days' WHERE id = $1`, ids[3]); err != nil {
		t.Fatal(err)
	}

	sending := e.otherKey("sending_access")
	if code, b := sending.call("GET", "/insights", "", nil); code != 403 {
		t.Fatalf("a sending key read insights: %d %v", code, b)
	}
	code, in := e.call("GET", "/insights", "", nil)
	if code != 200 || in["object"] != "insights" || in["emails_considered"].(float64) != 4 || in["truncated"] != false {
		t.Fatalf("insights: %d %v", code, in)
	}
	windows := in["windows"].([]any)
	if len(windows) != 3 {
		t.Fatalf("windows: %v", windows)
	}
	w24 := windows[0].(map[string]any)
	total := w24["total"].(map[string]any)
	if w24["window"] != "24h" || total["attempts"].(float64) != 4 || !insightNear(total["acceptance_rate"], 0.5) ||
		!insightNear(total["deferral_rate"], 0.25) || !insightNear(total["bounce_rate"], 0.25) {
		t.Fatalf("24h total: %v", total)
	}
	providers := w24["by_provider"].([]any)
	google := providers[0].(map[string]any)
	if google["provider"] != "google" || google["attempts"].(float64) != 3 || google["accepted"].(float64) != 2 ||
		!insightNear(google["time_to_acceptance_p50_seconds"], 90) || !insightNear(google["time_to_acceptance_p95_seconds"], 117) {
		t.Fatalf("google in 24h: %v", google)
	}
	if len(providers) != 2 || providers[1].(map[string]any)["provider"] != "microsoft" ||
		!insightNear(providers[1].(map[string]any)["bounce_rate"], 1) {
		t.Fatalf("24h providers: %v", providers)
	}
	ips := w24["by_sending_ip"].([]any)
	if len(ips) != 2 || ips[0].(map[string]any)["sending_ip"] != "203.0.113.7" || ips[0].(map[string]any)["attempts"].(float64) != 3 {
		t.Fatalf("24h sending IPs: %v", ips)
	}
	w30 := windows[2].(map[string]any)
	if w30["window"] != "30d" || w30["total"].(map[string]any)["attempts"].(float64) != 5 || len(w30["by_provider"].([]any)) != 3 {
		t.Fatalf("30d: %v", w30)
	}
	if store.ProviderOf("ALT2.ASPMX.L.GOOGLE.COM.") != "google" || store.ProviderOf("mx01.mail.icloud.com") != "apple" ||
		store.ProviderOf("") != "unknown" || store.ProviderOf("mx.example.org") != "other" {
		t.Fatal("ProviderOf")
	}
}

// otherKey creates another API key of this project with the given
// permission.
func (e *env) otherKey(permission string) *harness {
	e.t.Helper()
	code, b := e.call("POST", "/api-keys", "", map[string]any{"name": "k-" + permission, "permission": permission})
	if code != 201 {
		e.t.Fatalf("create %s key: %d %v", permission, code, b)
	}
	return &harness{t: e.t, st: e.st, url: e.url, apiKey: b["token"].(string)}
}
