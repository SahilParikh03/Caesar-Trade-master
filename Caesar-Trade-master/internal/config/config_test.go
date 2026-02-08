package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Env != "development" {
		t.Errorf("expected env=development, got %s", cfg.Env)
	}

	if cfg.Signer.SocketPath != "/var/run/caesar/signer.sock" {
		t.Errorf("unexpected socket path: %s", cfg.Signer.SocketPath)
	}

	if cfg.DB.Port != 5432 {
		t.Errorf("expected db port 5432, got %d", cfg.DB.Port)
	}

	if cfg.Redis.Addr != "localhost:6379" {
		t.Errorf("expected redis addr localhost:6379, got %s", cfg.Redis.Addr)
	}
}

func TestLoadFromEnv(t *testing.T) {
	os.Setenv("CAESAR_ENV", "production")
	os.Setenv("CAESAR_SIGNER_KMS_KEY_ID", "arn:aws:kms:us-east-1:123456:key/test-key")
	defer os.Unsetenv("CAESAR_ENV")
	defer os.Unsetenv("CAESAR_SIGNER_KMS_KEY_ID")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Env != "production" {
		t.Errorf("expected env=production, got %s", cfg.Env)
	}

	if cfg.Signer.KMSKeyID != "arn:aws:kms:us-east-1:123456:key/test-key" {
		t.Errorf("unexpected kms key id: %s", cfg.Signer.KMSKeyID)
	}
}

func TestDBDSN(t *testing.T) {
	cfg := DBConfig{
		Host:     "localhost",
		Port:     5432,
		User:     "caesar",
		Password: "secret",
		DBName:   "caesar",
		SSLMode:  "disable",
	}

	expected := "host=localhost port=5432 user=caesar password=secret dbname=caesar sslmode=disable"
	if cfg.DSN() != expected {
		t.Errorf("unexpected DSN:\ngot:  %s\nwant: %s", cfg.DSN(), expected)
	}
}
