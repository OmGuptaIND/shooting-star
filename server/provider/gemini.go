package provider

import (
	"context"

	"github.com/OmGuptaIND/shooting-star/config/env"
	"github.com/OmGuptaIND/shooting-star/config/logger"
	"github.com/google/generative-ai-go/genai"
	"go.uber.org/zap"
	"google.golang.org/api/option"
)

type GeminiModels string

const (
	// Embedding Exp 03 07
	EmbeddingExp0307 GeminiModels = "gemini-embedding-exp-03-07"

	// Text Embedding 004
	EmbeddingText004 GeminiModels = "text-embedding-004"

	// embedding-001
	Embedding001 GeminiModels = "embedding-001"

	// Gemini 1.5 Pro Latest
	Gemini1_5ProLatest GeminiModels = "gemini-1.5-pro-latest"

	// Gemini 2.5 Pro Latest
	Gemini2_5Pro GeminiModels = "gemini-2.5-pro-preview-03-25"

	// Gemini 2.0 Flash
	Gemini2_0Flash GeminiModels = "gemini-2.0-flash"
)

// geminiEmbedding is a struct that implements the Embedding interface
type geminiProvider struct {
	ctx context.Context

	// Google Gemini API client
	genai.Client

	logger *zap.Logger
}

// Create a new instance of geminiEmbedding
func NewGeminiProvider(ctx context.Context) (*geminiProvider, error) {
	l := logger.FromCtx(ctx)

	genClient, err := genai.NewClient(ctx, option.WithAPIKey(env.GetGeminiAPIKey()))
	if err != nil {
		l.Error("failed to create Gemini client", zap.Error(err))
		return nil, err
	}

	return &geminiProvider{
		ctx:    ctx,
		Client: *genClient,
		logger: logger.FromCtx(ctx),
	}, nil
}