#!/usr/bin/env bash
# Offline MLX lifecycle for Sentra Code Memory.
#
# Primary chat/VLM: LiquidAI LFM2.5-VL-1.6B MLX 8-bit.
# Chat fallback: Google Gemma 4 E2B IT MLX 4-bit.
# Embedding/reranking model IDs are exported for the local retrieval adapters.
set -euo pipefail

PORT="${SENTRA_CODE_MEMORY_MLX_PORT:-${OUROBOROS_BRAIN_MLX_PORT:-8080}}"
HOST="${SENTRA_CODE_MEMORY_MLX_HOST:-${OUROBOROS_BRAIN_MLX_HOST:-127.0.0.1}}"
MODEL="${SENTRA_CODE_MEMORY_MLX_CHAT_MODEL:-${OUROBOROS_BRAIN_MLX_CHAT_MODEL:-mlx-community/LFM2.5-VL-1.6B-8bit}}"
FALLBACK_MODEL="${SENTRA_CODE_MEMORY_MLX_CHAT_FALLBACK_MODEL:-${OUROBOROS_BRAIN_MLX_CHAT_FALLBACK_MODEL:-mlx-community/gemma-4-e2b-it-4bit}}"
EMBED_MODEL="${SENTRA_CODE_MEMORY_MLX_EMBED_MODEL:-${OUROBOROS_BRAIN_MLX_EMBED_MODEL:-mlx-community/Qwen3-VL-Embedding-2B-4bit}}"
RANK_MODEL="${SENTRA_CODE_MEMORY_MLX_RANK_MODEL:-${OUROBOROS_BRAIN_MLX_RANK_MODEL:-mlx-community/Qwen3-VL-Reranker-2B-4bit}}"
PID_FILE="${SENTRA_CODE_MEMORY_MLX_PID:-${OUROBOROS_BRAIN_MLX_PID:-$HOME/.sentra-code-memory/mlx-serve.pid}}"
MODEL_FILE="${SENTRA_CODE_MEMORY_MLX_MODEL_FILE:-${PID_FILE}.model}"
LOG_FILE="${SENTRA_CODE_MEMORY_MLX_LOG:-${OUROBOROS_BRAIN_MLX_LOG:-$HOME/.sentra-code-memory/mlx-serve.log}}"

mkdir -p "$(dirname "$PID_FILE")" "$(dirname "$LOG_FILE")"
cmd="${1:-start}"

health() {
	curl -sf "http://${HOST}:${PORT}/v1/models" >/dev/null 2>&1
}

server_module() {
	case "$1" in
	*VL* | *gemma-4*) echo "mlx_vlm.server" ;;
	*) echo "mlx_lm.server" ;;
	esac
}

start_model() {
	local model="$1"
	local module
	module="$(server_module "$model")"
	echo "starting $module model=$model port=$PORT"
	# shellcheck disable=SC2086
	nohup python3 -m "$module" \
		--model "$model" \
		--port "$PORT" \
		--host "$HOST" \
		>>"$LOG_FILE" 2>&1 &
	echo $! >"$PID_FILE"
	for _ in $(seq 1 30); do
		if health; then
			printf '%s\n' "$model" >"$MODEL_FILE"
			return 0
		fi
		sleep 1
	done
	kill "$(cat "$PID_FILE")" 2>/dev/null || true
	rm -f "$PID_FILE" "$MODEL_FILE"
	return 1
}

emit_env() {
	local active="${1:-$MODEL}"
	cat <<EOF
export OUROBOROS_BRAIN_LLM=mlx
export OUROBOROS_BRAIN_EMBED=mlx
export OUROBOROS_BRAIN_RANKER=mlx
export OUROBOROS_BRAIN_MLX_BASE_URL=http://${HOST}:${PORT}/v1
export OUROBOROS_BRAIN_MLX_CHAT_MODEL=${active}
export OUROBOROS_BRAIN_MLX_CHAT_FALLBACK_MODEL=${FALLBACK_MODEL}
export OUROBOROS_BRAIN_MLX_EMBED_MODEL=${EMBED_MODEL}
export OUROBOROS_BRAIN_MLX_RANK_MODEL=${RANK_MODEL}
export SENTRA_CODE_MEMORY_MLX_BASE_URL=http://${HOST}:${PORT}/v1
export SENTRA_CODE_MEMORY_MLX_CHAT_MODEL=${active}
export SENTRA_CODE_MEMORY_MLX_CHAT_FALLBACK_MODEL=${FALLBACK_MODEL}
export SENTRA_CODE_MEMORY_MLX_EMBED_MODEL=${EMBED_MODEL}
export SENTRA_CODE_MEMORY_MLX_RANK_MODEL=${RANK_MODEL}
EOF
}

case "$cmd" in
start)
	if health; then
		active="$(cat "$MODEL_FILE" 2>/dev/null || printf '%s' "$MODEL")"
		echo "mlx already healthy at http://${HOST}:${PORT}/v1 model=$active"
		exit 0
	fi
	if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
		echo "mlx pid alive but not healthy yet; log: $LOG_FILE"
		exit 0
	fi
	if ! command -v python3 >/dev/null; then
		echo "python3 required for mlx-lm/mlx-vlm server" >&2
		exit 2
	fi
	if start_model "$MODEL"; then
		echo "mlx ready http://${HOST}:${PORT}/v1 model=$MODEL"
		emit_env "$MODEL"
		exit 0
	fi
	echo "primary MLX model failed; trying fallback=$FALLBACK_MODEL" >&2
	if [[ "$FALLBACK_MODEL" != "$MODEL" ]] && start_model "$FALLBACK_MODEL"; then
		echo "mlx ready http://${HOST}:${PORT}/v1 model=$FALLBACK_MODEL"
		emit_env "$FALLBACK_MODEL"
		exit 0
	fi
	echo "mlx start timeout; see $LOG_FILE" >&2
	exit 1
	;;
stop)
	if [[ -f "$PID_FILE" ]]; then
		kill "$(cat "$PID_FILE")" 2>/dev/null || true
		rm -f "$PID_FILE" "$MODEL_FILE"
		echo "mlx stopped"
	else
		echo "no pid file"
	fi
	;;
status)
	if health; then
		active="$(cat "$MODEL_FILE" 2>/dev/null || printf '%s' "$MODEL")"
		echo "healthy http://${HOST}:${PORT}/v1 model=$active"
		curl -s "http://${HOST}:${PORT}/v1/models" | head -c 400
		echo
		exit 0
	fi
	echo "unhealthy"
	exit 1
	;;
env)
	active="$(cat "$MODEL_FILE" 2>/dev/null || printf '%s' "$MODEL")"
	emit_env "$active"
	;;
*)
	echo "usage: $0 start|stop|status|env" >&2
	exit 2
	;;
esac
