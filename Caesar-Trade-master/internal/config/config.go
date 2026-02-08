package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// Config holds all application configuration.
type Config struct {
	Env                string `mapstructure:"env"`
	LocalStackEndpoint string `mapstructure:"localstack_endpoint"`
	Signer             SignerConfig
	DB                 DBConfig
	Redis              RedisConfig
}

// SignerConfig holds signer-specific settings.
type SignerConfig struct {
	SocketPath    string `mapstructure:"socket_path"`
	SessionTTLSec int    `mapstructure:"session_ttl_sec"`
	KMSKeyID      string `mapstructure:"kms_key_id"`
	AWSRegion     string `mapstructure:"aws_region"`
}

// DBConfig holds PostgreSQL connection settings.
type DBConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	DBName   string `mapstructure:"dbname"`
	SSLMode  string `mapstructure:"sslmode"`
}

// DSN returns the PostgreSQL connection string.
func (d DBConfig) DSN() string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.DBName, d.SSLMode)
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// Load reads configuration from environment variables prefixed with CAESAR_.
func Load() (*Config, error) {
	v := viper.New()
	v.SetEnvPrefix("CAESAR")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Defaults
	v.SetDefault("env", "development")

	// Signer defaults
	v.SetDefault("signer.socket_path", "/var/run/caesar/signer.sock")
	v.SetDefault("signer.session_ttl_sec", 3600)
	v.SetDefault("signer.aws_region", "us-east-1")

	// DB defaults
	v.SetDefault("db.host", "localhost")
	v.SetDefault("db.port", 5432)
	v.SetDefault("db.user", "caesar")
	v.SetDefault("db.password", "caesar")
	v.SetDefault("db.dbname", "caesar")
	v.SetDefault("db.sslmode", "disable")

	// Redis defaults
	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)

	cfg := &Config{}

	cfg.Env = v.GetString("env")
	cfg.LocalStackEndpoint = v.GetString("localstack_endpoint")

	cfg.Signer = SignerConfig{
		SocketPath:    v.GetString("signer.socket_path"),
		SessionTTLSec: v.GetInt("signer.session_ttl_sec"),
		KMSKeyID:      v.GetString("signer.kms_key_id"),
		AWSRegion:     v.GetString("signer.aws_region"),
	}

	cfg.DB = DBConfig{
		Host:     v.GetString("db.host"),
		Port:     v.GetInt("db.port"),
		User:     v.GetString("db.user"),
		Password: v.GetString("db.password"),
		DBName:   v.GetString("db.dbname"),
		SSLMode:  v.GetString("db.sslmode"),
	}

	cfg.Redis = RedisConfig{
		Addr:     v.GetString("redis.addr"),
		Password: v.GetString("redis.password"),
		DB:       v.GetInt("redis.db"),
	}

	return cfg, nil
}
