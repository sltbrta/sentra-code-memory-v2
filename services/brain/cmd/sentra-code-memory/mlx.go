package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// runMLX exposes the offline MLX lifecycle without making model serving a
// requirement for local code indexing. The shell script owns process/PID/log
// handling so the same lifecycle works from source and from a built binary.
func runMLX(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "mlx: start|stop|status|env required")
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("mlx-"+sub, flag.ContinueOnError)
	fs.SetOutput(errOut)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, "mlx does not accept positional arguments")
		return 2
	}
	switch sub {
	case "start", "stop", "status", "env":
	default:
		fmt.Fprintf(errOut, "mlx: unknown subcommand %q; use start|stop|status|env\n", sub)
		return 2
	}
	if script := findMLXScript(); script != "" {
		cmd := exec.Command(script, sub)
		cmd.Stdout = out
		cmd.Stderr = errOut
		cmd.Env = os.Environ()
		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode()
			}
			fmt.Fprintln(errOut, err)
			return 1
		}
		return 0
	}
	if sub == "env" {
		base := os.Getenv("OUROBOROS_BRAIN_MLX_BASE_URL")
		if base == "" {
			base = "http://127.0.0.1:8080/v1"
		}
		fmt.Fprintf(out, "export OUROBOROS_BRAIN_LLM=mlx\nexport OUROBOROS_BRAIN_EMBED=mlx\nexport OUROBOROS_BRAIN_RANKER=mlx\nexport OUROBOROS_BRAIN_MLX_BASE_URL=%s\nexport OUROBOROS_BRAIN_MLX_CHAT_MODEL=%s\nexport OUROBOROS_BRAIN_MLX_CHAT_FALLBACK_MODEL=%s\nexport OUROBOROS_BRAIN_MLX_EMBED_MODEL=%s\nexport OUROBOROS_BRAIN_MLX_RANK_MODEL=%s\n", base, mlxChatModel(), mlxChatFallbackModel(), mlxEmbedModel(), mlxRankModel())
		return 0
	}
	if sub == "status" {
		base := os.Getenv("OUROBOROS_BRAIN_MLX_BASE_URL")
		if base == "" {
			base = "http://127.0.0.1:8080/v1"
		}
		if mlxHealth(base) {
			fmt.Fprintf(out, "healthy %s\n", base)
			return 0
		}
		fmt.Fprintf(errOut, "unhealthy %s\n", base)
		return 1
	}
	fmt.Fprintln(errOut, "mlx: scripts/mlx-serve.sh not found; run from the repository root")
	return 2
}

func findMLXScript() string {
	candidates := []string{
		os.Getenv("SENTRA_CODE_MEMORY_MLX_SCRIPT"),
		"scripts/mlx-serve.sh",
		filepath.Join("..", "..", "..", "..", "scripts", "mlx-serve.sh"),
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "mlx-serve.sh"),
			filepath.Join(dir, "..", "scripts", "mlx-serve.sh"),
		)
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			absolute, err := filepath.Abs(candidate)
			if err == nil {
				return absolute
			}
		}
	}
	return ""
}

func mlxChatModel() string {
	if model := os.Getenv("OUROBOROS_BRAIN_MLX_CHAT_MODEL"); model != "" {
		return model
	}
	return "mlx-community/LFM2.5-VL-1.6B-8bit"
}

func mlxChatFallbackModel() string {
	if model := os.Getenv("OUROBOROS_BRAIN_MLX_CHAT_FALLBACK_MODEL"); model != "" {
		return model
	}
	return "mlx-community/gemma-4-e2b-it-4bit"
}

func mlxEmbedModel() string {
	if model := os.Getenv("OUROBOROS_BRAIN_MLX_EMBED_MODEL"); model != "" {
		return model
	}
	return "mlx-community/Qwen3-VL-Embedding-2B-4bit"
}

func mlxRankModel() string {
	if model := os.Getenv("OUROBOROS_BRAIN_MLX_RANK_MODEL"); model != "" {
		return model
	}
	return "mlx-community/Qwen3-VL-Reranker-2B-4bit"
}

func mlxHealth(base string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/models")
	if err != nil {
		resp, err = client.Get(base)
		if err != nil {
			return false
		}
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}
