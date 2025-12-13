// internal/config/config.go
package config

import (
	"webBridgeBot/internal/logger"
	"webBridgeBot/internal/reader"

	"github.com/spf13/viper"
)

// DefaultChunkSize is the preferred chunk size for internal processing and caching.
// It must be a multiple of 4096. To avoid potential 'LIMIT_INVALID' errors
// when the Telegram API's upload.getFile method is called, we select a value
// that is 4096 multiplied by a power of 2 (e.g., 64).
// This value is 256 KiB (262144 bytes).
const DefaultChunkSize int64 = 256 * 1024 // 256 KB

type Configuration struct {
	// Telegram
	ApiID    int
	ApiHash  string
	BotToken string

	// Web Server
	BaseURL    string
	Port       string
	HashLength int

	// Cache
	CacheDirectory string
	MaxCacheSize   int64

	// Supabase (PostgreSQL) - NUEVOS CAMPOS
	SupabaseHost     string
	SupabasePort     string
	SupabaseUser     string
	SupabasePassword string
	SupabaseDatabase string

	// Otros
	DebugMode    bool
	LogLevel     string // Log level: DEBUG, INFO, WARNING, ERROR
	LogChannelID string

	// Connection and retry settings
	RequestTimeout int // Timeout for Telegram API requests in seconds
	MaxRetries     int // Maximum number of retry attempts for failed requests
	RetryBaseDelay int // Base delay for exponential backoff in seconds
	MaxRetryDelay  int // Maximum retry delay in seconds

	// Interno
	BinaryCache *reader.BinaryCache
}

// InitializeViper sets up Viper to read from environment variables and the .env file.
// This function should be called early in main.
func InitializeViper(log *logger.Logger) {
	viper.AutomaticEnv() // Read environment variables (e.g., from docker-compose)

	// Explicitly set the config file name and type for .env
	viper.SetConfigFile(".env")
	viper.AddConfigPath(".") // Look for .env in the current directory

	if err := viper.ReadInConfig(); err != nil {
		// Log a warning if .env not found. This is normal if config comes from env vars or flags.
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Info(".env config file not found (this is expected if configuration is solely via environment variables or command-line flags).")
		} else {
			log.Infof("Could not read .env file: %v", err)
			log.Info("Hint: If you need to use a .env file, copy env.sample to .env and configure it.")
		}
		log.Info("Configuration will be loaded from environment variables and command-line flags.")
	} else {
		log.Info("Successfully loaded configuration from .env file")
	}
}

// LoadConfig loads configuration from Viper's resolved settings.
func LoadConfig(log *logger.Logger) Configuration {
	var cfg Configuration

	// Telegram
	cfg.ApiID = viper.GetInt("API_ID")
	cfg.ApiHash = viper.GetString("API_HASH")
	cfg.BotToken = viper.GetString("BOT_TOKEN")

	// Web Server
	cfg.BaseURL = viper.GetString("BASE_URL")
	cfg.Port = viper.GetString("PORT")
	cfg.HashLength = viper.GetInt("HASH_LENGTH")

	// Cache
	cfg.CacheDirectory = viper.GetString("CACHE_DIRECTORY")
	cfg.MaxCacheSize = viper.GetInt64("MAX_CACHE_SIZE")

	// Supabase - NUEVO: carga desde Viper
	cfg.SupabaseHost = viper.GetString("SUPABASE_HOST")
	cfg.SupabasePort = viper.GetString("SUPABASE_PORT")
	cfg.SupabaseUser = viper.GetString("SUPABASE_USER")
	cfg.SupabasePassword = viper.GetString("SUPABASE_PASSWORD")
	cfg.SupabaseDatabase = viper.GetString("SUPABASE_DATABASE")

	// Otros
	cfg.DebugMode = viper.GetBool("DEBUG_MODE")
	cfg.LogLevel = viper.GetString("LOG_LEVEL")
	cfg.LogChannelID = viper.GetString("LOG_CHANNEL_ID")

	// Connection settings
	cfg.RequestTimeout = viper.GetInt("REQUEST_TIMEOUT")
	cfg.MaxRetries = viper.GetInt("MAX_RETRIES")
	cfg.RetryBaseDelay = viper.GetInt("RETRY_BASE_DELAY")
	cfg.MaxRetryDelay = viper.GetInt("MAX_RETRY_DELAY")

	// Aplicar valores por defecto
	setDefaultValues(&cfg)

	// Validar campos obligatorios (incluyendo Supabase)
	validateMandatoryFields(cfg, log)

	// Inicializar caché binario
	initializeBinaryCache(&cfg, log)

	if cfg.DebugMode {
		// No imprimir password en debug
		safeCfg := cfg
		safeCfg.SupabasePassword = "*****"
		log.Debugf("Loaded configuration: %+v", safeCfg)
	}

	return cfg
}

func validateMandatoryFields(cfg Configuration, log *logger.Logger) {
	if cfg.ApiID == 0 {
		log.Fatal("API_ID is required and not set")
	}
	if cfg.ApiHash == "" {
		log.Fatal("API_HASH is required and not set")
	}
	if cfg.BotToken == "" {
		log.Fatal("BOT_TOKEN is required and not set")
	}
	if cfg.BaseURL == "" {
		log.Fatal("BASE_URL is required and not set")
	}
	// Validación Supabase
	if cfg.SupabaseHost == "" {
		log.Fatal("SUPABASE_HOST is required")
	}
	if cfg.SupabaseUser == "" {
		log.Fatal("SUPABASE_USER is required")
	}
	if cfg.SupabasePassword == "" {
		log.Fatal("SUPABASE_PASSWORD is required")
	}
}

func setDefaultValues(cfg *Configuration) {
	if cfg.HashLength < 6 {
		cfg.HashLength = 8
	}
	if cfg.CacheDirectory == "" {
		cfg.CacheDirectory = ".cache"
	}
	if cfg.MaxCacheSize == 0 {
		cfg.MaxCacheSize = 10 * 1024 * 1024 * 1024 // 10 GB
	}
	if cfg.Port == "" {
		cfg.Port = "8080"
	}
	if cfg.LogLevel == "" {
		if cfg.DebugMode {
			cfg.LogLevel = "DEBUG"
		} else {
			cfg.LogLevel = "INFO"
		}
	}

	// Supabase defaults
	if cfg.SupabasePort == "" {
		cfg.SupabasePort = "5432"
	}
	if cfg.SupabaseDatabase == "" {
		cfg.SupabaseDatabase = "postgres"
	}

	// Connection defaults
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 300
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 5
	}
	if cfg.RetryBaseDelay == 0 {
		cfg.RetryBaseDelay = 1
	}
	if cfg.MaxRetryDelay == 0 {
		cfg.MaxRetryDelay = 60
	}
}

func initializeBinaryCache(cfg *Configuration, log *logger.Logger) {
	var err error
	cfg.BinaryCache, err = reader.NewBinaryCache(
		cfg.CacheDirectory,
		cfg.MaxCacheSize,
		DefaultChunkSize,
	)
	if err != nil {
		log.Fatalf("Error initializing BinaryCache: %v", err)
	}
}
