package oracle

import "testing"

func TestKeyClass(t *testing.T) {
	for key, want := range map[string]string{
		"tls.crt": ClassTLS, "tls.key": ClassTLS, "ca.crt": ClassTLS, "server.pem": ClassTLS, "webhook.crt": ClassTLS,
		"cert.cer": ClassTLS, "fullchain.pem": ClassTLS,
		"password": ClassCredential, "postgres-password": ClassCredential, "admin-password": ClassCredential,
		"jwtSecret": ClassCredential, "encryptionKey": ClassCredential, "storage-key": ClassCredential,
		"haSharedSecret": ClassCredential, "API_TOKEN": ClassCredential, "encryption.key": ClassCredential,
		// A private key is a credential however it is named, PEM or keystore (review item 1).
		"key.pem": ClassCredential, "privkey.pem": ClassCredential, "signing-key.pem": ClassCredential,
		"keystore.jks": ClassCredential, "store.p12": ClassCredential,
		// Credentials that read as "other" before (review item 3).
		"DATABASE_URL": ClassCredential, "connection-string": ClassCredential, "id_rsa": ClassCredential,
		".dockerconfigjson": ClassCredential, "erlang-cookie": ClassCredential, "redis-pass": ClassCredential,
		// Plurals, an uppercase run before a word, and run-together names (review round 2).
		"mongodb-passwords": ClassCredential, "api-tokens": ClassCredential, "keys": ClassCredential,
		"DBPassword": ClassCredential, "JWTSecret": ClassCredential, "htpasswd": ClassCredential,
		"secretkey": ClassCredential, "dbpassword": ClassCredential, "DBPass": ClassCredential, "JWTKey": ClassCredential,
		// Bare substrings must not make ordinary keys credentials (review item 2).
		"keyspace": ClassOther, "author": ClassOther, "monkey": ClassOther,
		"username": ClassOther, "database": ClassOther, "config.yaml": ClassOther, "host": ClassOther,
		"monkeys": ClassOther, "authors": ClassOther, "hosts": ClassOther, "status": ClassOther, "accessMode": ClassOther, "HTTPPort": ClassOther,
	} {
		if got := KeyClass(key); got != want {
			t.Errorf("KeyClass(%q) = %s, want %s", key, got, want)
		}
	}
}
