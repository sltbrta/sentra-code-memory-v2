package hosted

import "testing"

func TestStandaloneMLXDefaultsUseLocalMultimodalModels(t *testing.T) {
	for _, name := range []string{
		"SENTRA_CODE_MEMORY_MLX_CHAT_MODEL",
		"OUROBOROS_BRAIN_MLX_CHAT_MODEL",
		"SENTRA_CODE_MEMORY_MLX_CHAT_FALLBACK_MODEL",
		"OUROBOROS_BRAIN_MLX_CHAT_FALLBACK_MODEL",
		"SENTRA_CODE_MEMORY_MLX_EMBED_MODEL",
		"OUROBOROS_BRAIN_MLX_EMBED_MODEL",
		"SENTRA_CODE_MEMORY_MLX_RANK_MODEL",
		"OUROBOROS_BRAIN_MLX_RANK_MODEL",
	} {
		t.Setenv(name, "")
	}
	if got := mlxChatConfig().Model; got != "mlx-community/LFM2.5-VL-1.6B-8bit" {
		t.Fatalf("chat model=%q", got)
	}
	if got := mlxChatFallbackModel(); got != "mlx-community/gemma-4-e2b-it-4bit" {
		t.Fatalf("fallback model=%q", got)
	}
	if got := mlxEmbedConfig().Model; got != "mlx-community/Qwen3-VL-Embedding-2B-4bit" {
		t.Fatalf("embed model=%q", got)
	}
	if got := mlxRankModel(); got != "mlx-community/Qwen3-VL-Reranker-2B-4bit" {
		t.Fatalf("rank model=%q", got)
	}
}

func TestStandaloneMLXSettingsOverrideLegacySettings(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_MLX_CHAT_MODEL", "legacy-chat")
	t.Setenv("SENTRA_CODE_MEMORY_MLX_CHAT_MODEL", "standalone-chat")
	t.Setenv("OUROBOROS_BRAIN_MLX_BASE_URL", "http://legacy/v1")
	t.Setenv("SENTRA_CODE_MEMORY_MLX_BASE_URL", "http://standalone/v1")
	if got := mlxChatConfig().Model; got != "standalone-chat" {
		t.Fatalf("chat model=%q", got)
	}
	if got := mlxChatConfig().BaseURL; got != "http://standalone/v1" {
		t.Fatalf("base URL=%q", got)
	}
}
