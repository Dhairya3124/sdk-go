package main

import (
	"net/http"
	"time"
)

type LocaleCode string

type EngineConfig struct {
	APIKey             string
	APIURL             string
	BatchSize          int
	IdealBatchItemSize int
}
type LingoDotDevEngine struct {
	config     *EngineConfig
	httpClient *http.Client
}

func NewEngine(config *EngineConfig) *LingoDotDevEngine {
	if config.APIURL == "" {
		config.APIURL = "https://engine.lingo.dev"
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 25
	}
	if config.IdealBatchItemSize <= 0 {
		config.IdealBatchItemSize = 250
	}
	return &LingoDotDevEngine{
		config: config,
		httpClient: &http.Client{
			Timeout: 300 * time.Second,
		},
	}
}

