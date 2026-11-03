package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubAppMintsOneRepositoryReadOnlyTokens(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var mints atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/Acme/backend/installation":
			verifyAppJWT(t, r.Header.Get("Authorization"), &key.PublicKey)
			_, _ = w.Write([]byte(`{"id":42}`))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			verifyAppJWT(t, r.Header.Get("Authorization"), &key.PublicKey)
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if strings.Join(body.Repositories, ",") != "backend" {
				t.Errorf("token scoped to %v, want only backend", body.Repositories)
			}
			if len(body.Permissions) != 2 || body.Permissions["contents"] != "read" || body.Permissions["metadata"] != "read" {
				t.Errorf("permissions = %v, want contents and metadata read", body.Permissions)
			}
			mints.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour)})
		case r.URL.Path == "/repos/Acme/backend/commits/0123456":
			if r.Header.Get("Authorization") != "token ghs_x" {
				t.Errorf("commit read with %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"sha":"` + testSHA + `"}`))
		case strings.HasPrefix(r.URL.Path, "/repos/Acme/backend/commits/"):
			http.Error(w, `{"message":"No commit found"}`, http.StatusUnprocessableEntity)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	g := newGitHubApp("123", key, api.URL)
	sha, err := g.resolveCommit(context.Background(), "Acme/backend", "0123456")
	if err != nil || sha != testSHA {
		t.Fatalf("resolveCommit = %q, %v", sha, err)
	}
	if _, err := g.token(context.Background(), "Acme/backend"); err != nil {
		t.Fatal(err)
	}
	if mints.Load() != 1 {
		t.Errorf("minted %d tokens, want the server's cached one reused", mints.Load())
	}
	if _, err := g.reviewToken(context.Background(), "Acme/backend"); err != nil {
		t.Fatal(err)
	}
	if mints.Load() != 2 {
		t.Errorf("minted %d tokens, want a fresh one for the review", mints.Load())
	}
	if _, err := g.resolveCommit(context.Background(), "Acme/backend", "fedcba9"); !errors.Is(err, errCommitNotFound) {
		t.Errorf("unpushed commit: err = %v, want errCommitNotFound", err)
	}
}

func verifyAppJWT(t *testing.T, auth string, pub *rsa.PublicKey) {
	t.Helper()
	jwt, ok := strings.CutPrefix(auth, "Bearer ")
	parts := strings.Split(jwt, ".")
	if !ok || len(parts) != 3 {
		t.Fatalf("authorization %q is not an App JWT", auth)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("JWT signature: %v", err)
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	_ = json.Unmarshal(claims, &c)
	if c.Iss != "123" || c.Exp-c.Iat > 600 {
		t.Fatalf("claims = %+v", c)
	}
}

// A repository the App cannot find is errRepositoryNotFound, so the server
// answers the caller rather than counting an error.
func TestGitHubAppRepository(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/Acme/radar/installation":
			_, _ = w.Write([]byte(`{"id":42}`))
		case r.URL.Path == "/app/installations/42/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour)})
		case r.URL.Path == "/repos/Acme/radar":
			_, _ = w.Write([]byte(`{"full_name":"Acme/radar","archived":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	g := newGitHubApp("123", key, api.URL)
	info, err := g.repository(context.Background(), "Acme/radar")
	if err != nil || info.FullName != "Acme/radar" || !info.Archived {
		t.Fatalf("repository = %+v, %v", info, err)
	}
	if _, err := g.repository(context.Background(), "Acme/missing"); !errors.Is(err, errRepositoryNotFound) {
		t.Fatalf("missing repository: err = %v, want errRepositoryNotFound", err)
	}
}
