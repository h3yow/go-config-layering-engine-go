package config

import (
	"context"
	"testing"
	"time"
)

type appConfig struct {
	Server struct {
		Host string `config:"host"`
		Port int    `config:"port"`
	} `config:"server"`
	Log struct {
		Level string `config:"level"`
	} `config:"log"`
	DB struct {
		DSN string `config:"dsn"`
	} `config:"db"`
	HTTP struct {
		Timeout time.Duration `config:"timeout"`
	} `config:"http"`
}

func TestDecodeIntoStruct(t *testing.T) {
	m, err := NewManager(context.Background(), testSchema(),
		NewMapSource("file", PriorityFile, map[string]string{
			"db.dsn":       "pg://x",
			"server.port":  "8081",
			"http.timeout": "10s",
		}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	var cfg appConfig
	if err := m.Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	t.Logf("decoded: %+v", cfg)
	if cfg.Server.Port != 8081 {
		t.Fatalf("port: want 8081, got %d", cfg.Server.Port)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("host default: want 127.0.0.1, got %q", cfg.Server.Host)
	}
	if cfg.DB.DSN != "pg://x" {
		t.Fatalf("dsn: want pg://x, got %q", cfg.DB.DSN)
	}
	if cfg.HTTP.Timeout != 10*time.Second {
		t.Fatalf("timeout: want 10s, got %v", cfg.HTTP.Timeout)
	}
	if cfg.Log.Level != "info" {
		t.Fatalf("level default: want info, got %q", cfg.Log.Level)
	}
}

func TestDecodeRejectsNonPointer(t *testing.T) {
	m, err := NewManager(context.Background(), testSchema(),
		NewMapSource("file", PriorityFile, map[string]string{"db.dsn": "x"}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	var cfg appConfig
	if err := m.Decode(cfg); err == nil {
		t.Fatal("expected error for non-pointer decode target")
	} else {
		t.Logf("non-pointer decode rejected: %v", err)
	}
}
