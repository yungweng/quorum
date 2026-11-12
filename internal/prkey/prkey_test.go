package prkey

import "testing"

func TestKeyRoundTrip(t *testing.T) {
	key := Format("acme/api", 42)
	if key != "acme/api#42" {
		t.Fatalf("key = %q", key)
	}
	if Repo(key) != "acme/api" || Number(key) != 42 || Name(key) != "api" {
		t.Fatalf("parsed %q as repo %q, number %d, name %q", key, Repo(key), Number(key), Name(key))
	}
}

func TestBranchKeyHasNoNumber(t *testing.T) {
	key := "acme/api#branch:feature/login"
	if Repo(key) != "acme/api" || Number(key) != 0 {
		t.Fatalf("parsed %q as repo %q, number %d", key, Repo(key), Number(key))
	}
}

func TestMalformedKey(t *testing.T) {
	if Repo("acme/api") != "acme/api" || Number("acme/api") != 0 {
		t.Fatal("a key without a number must keep its repo and report number 0")
	}
}
