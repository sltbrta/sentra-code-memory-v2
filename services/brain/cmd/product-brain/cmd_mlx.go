// product-brain mlx lifecycle helpers (BYOC local inference).
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func runMLX(args []string) {
	if len(args) < 1 {
		fatal("mlx: start|stop|status|env required")
	}
	sub := args[0]
	fs := flag.NewFlagSet("mlx-"+sub, flag.ExitOnError)
	_ = fs.Parse(args[1:])
	script := findMLXScript()
	switch sub {
	case "start", "stop", "status", "env":
		if script == "" {
			// Fallback: pure status/env without script.
			if sub == "env" {
				base := os.Getenv("OUROBOROS_BRAIN_MLX_BASE_URL")
				if base == "" {
					base = "http://127.0.0.1:8080/v1"
				}
				fmt.Printf("export OUROBOROS_BRAIN_LLM=mlx\nexport OUROBOROS_BRAIN_EMBED=mlx\nexport OUROBOROS_BRAIN_RANKER=mlx\nexport OUROBOROS_BRAIN_MLX_BASE_URL=%s\n", base)
				return
			}
			if sub == "status" {
				base := os.Getenv("OUROBOROS_BRAIN_MLX_BASE_URL")
				if base == "" {
					base = "http://127.0.0.1:8080/v1"
				}
				ok := mlxHealth(base)
				emitJSON(map[string]any{
					"event": "mlx_status", "healthy": ok, "base": base, "product_owned": true,
				})
				if !ok {
					os.Exit(1)
				}
				return
			}
			fatal("mlx: scripts/mlx-serve.sh not found; run from repo root or install script")
		}
		cmd := exec.Command(script, sub)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = os.Environ()
		if err := cmd.Run(); err != nil {
			fatal(err.Error())
		}
	default:
		fatal("mlx: unknown subcommand " + sub)
	}
}

func findMLXScript() string {
	// Prefer CWD and common monorepo roots.
	cands := []string{
		"scripts/mlx-serve.sh",
		filepath.Join("..", "scripts", "mlx-serve.sh"),
		filepath.Join("..", "..", "scripts", "mlx-serve.sh"),
	}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "mlx-serve.sh"))
	}
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	return ""
}

func mlxHealth(base string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/models")
	if err != nil {
		// try without double /v1
		resp, err = client.Get(base)
		if err != nil {
			return false
		}
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}
