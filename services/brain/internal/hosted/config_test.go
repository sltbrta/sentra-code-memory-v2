package hosted

import (
	"os"
	"testing"
)

func TestEnabledFalseWithoutSecrets(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_HOSTED", "")
	t.Setenv("OUROBOROS_ERB_HOSTED", "")
	t.Setenv("NEON_DATABASE_URL", "")
	t.Setenv("QDRANT_URL", "")
	t.Setenv("QDRANT_API_KEY", "")
	if Enabled() {
		t.Fatal("expected disabled without secrets")
	}
}

func TestEnabledExplicit(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_HOSTED", "1")
	if !Enabled() {
		t.Fatal("expected enabled")
	}
}

func TestEnabledExplicitFalseOverridesSecrets(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_HOSTED", "0")
	t.Setenv("OUROBOROS_ERB_HOSTED", "")
	t.Setenv("NEON_DATABASE_URL", "postgres://example")
	t.Setenv("QDRANT_URL", "https://qdrant.example")
	t.Setenv("QDRANT_API_KEY", "key")
	if Enabled() {
		t.Fatal("explicit local mode must override hosted credentials")
	}
}

func TestFromEnvMissing(t *testing.T) {
	for _, k := range []string{"NEON_DATABASE_URL", "DATABASE_URL", "QDRANT_URL", "QDRANT_API_KEY"} {
		_ = os.Unsetenv(k)
	}
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error")
	}
}

func TestOrTSQuery(t *testing.T) {
	q := orTSQuery("What is ACME RTO?", 10)
	if q == "" || q == "What" {
		t.Fatalf("bad tsquery %q", q)
	}
}

func TestSynthTemperatureDefault(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_TEMPERATURE", "")
	if got := SynthTemperature(0.1); got != 0.1 {
		t.Fatalf("expected fallback 0.1, got %v", got)
	}
	if got := SynthTemperature(0); got != 0 {
		t.Fatalf("expected fallback 0, got %v", got)
	}
}

func TestSynthTemperatureEnvOverride(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_TEMPERATURE", "0.7")
	if got := SynthTemperature(0.1); got != 0.7 {
		t.Fatalf("expected 0.7, got %v", got)
	}
}

func TestSynthTemperatureEnvInvalid(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_TEMPERATURE", "not-a-number")
	if got := SynthTemperature(0.1); got != 0.1 {
		t.Fatalf("expected fallback 0.1, got %v", got)
	}
}

func TestSynthSeedDefault(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_SEED", "")
	if got := SynthSeed(); got != nil {
		t.Fatalf("expected nil seed, got %v", *got)
	}
}

func TestSynthSeedEnvOverride(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_SEED", "42")
	got := SynthSeed()
	if got == nil {
		t.Fatal("expected non-nil seed")
		return
	}
	if *got != 42 {
		t.Fatalf("expected 42, got %v", *got)
	}
}

func TestSynthSeedEnvInvalid(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_SEED", "not-a-number")
	if got := SynthSeed(); got != nil {
		t.Fatalf("expected nil, got %v", *got)
	}
}

func TestProviderSupportsSeed(t *testing.T) {
	for _, name := range []string{"openai", "openrouter", "gemini"} {
		if !ProviderSupportsSeed(name) {
			t.Fatalf("expected %s to support seed", name)
		}
	}
	for _, name := range []string{"cerebras", "groq", "anthropic", "mlx", ""} {
		if ProviderSupportsSeed(name) {
			t.Fatalf("expected %s to NOT support seed", name)
		}
	}
}
