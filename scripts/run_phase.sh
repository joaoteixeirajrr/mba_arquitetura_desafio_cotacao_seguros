#!/usr/bin/env bash
# Orquestra uma execução de evidência (antes|depois) e captura as três séries.
#
# Perfil de carga desenhado para que os scrapes de 5s do Prometheus registrem:
#   - o DEGRAU do breaker closed->open->half_open->closed (o half_open é curto, então
#     na recuperação mandamos um filete lento de tráfego para ele PERMANECER em
#     half_open atravessando ao menos um scrape antes de fechar);
#   - uma curva de HIT RATE que sobe (fase de aquecimento com cotações repetidas);
#   - o p95 do POST /quotes sob a carga padrão do make reproduce.
#
# Não derruba o ambiente no fim (Prometheus retém 1h; o make down é do próximo passo).
set -euo pipefail

PHASE="${1:?uso: run_phase.sh <antes|depois>}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
PY="$ROOT/.venv-plot/bin/python"

load() { make load ARGS="$*" || true; }

echo "=== [$PHASE] make down && make reproduce ==="
make down
START_EPOCH="$(date +%s)"
make reproduce   # carga padrão: 10 baseline + 200 em voo (gera o p95 de ponta)

echo "=== [$PHASE] abrindo o breaker (rajada concorrente, cotações distintas) ==="
load -baseline 0 -requests 400 -concurrency 20 -distinct 30 -duration 40s

echo "=== [$PHASE] cooldown do breaker (30s) ==="
sleep 34

echo "=== [$PHASE] limpando o cache para os probes de recuperação chegarem às parceiras ==="
docker exec cotacao-redis redis-cli FLUSHALL >/dev/null 2>&1 || true

echo "=== [$PHASE] recuperação lenta: filete de tráfego para o half_open durar um scrape ==="
# Cada request precisa ser um cache MISS para de fato testar a parceira (um hit pula as
# parceiras — resilient.go passo 1). Esvaziamos o Redis ANTES de cada request e mandamos
# 1 por vez com gap > scrape (5s): o 1º sucesso leva o breaker a half_open e o scrape
# seguinte registra o estado 2 no gauge antes de o próximo request fechá-lo com o 2º sucesso.
for i in $(seq 1 12); do
  docker exec cotacao-redis redis-cli FLUSHALL >/dev/null 2>&1 || true
  load -baseline 0 -requests 1 -concurrency 1 -distinct 1 -duration 10s
  sleep 9
done

echo "=== [$PHASE] aquecendo o cache (cotações repetidas) para o hit rate subir ==="
# Mesmas poucas cotações repetidas várias vezes: os primeiros são miss, o resto hit,
# então a razão hit/(hit+miss) sobe visivelmente ao longo da janela.
for i in 1 2 3 4 5; do
  load -baseline 0 -requests 60 -concurrency 6 -distinct 3 -duration 20s
  sleep 3
done

# Deixa o Prometheus raspar o fim da janela (folga de 2 scrapes).
sleep 12
END_EPOCH="$(date +%s)"

mkdir -p docs/evidencias/graficos
echo "$START_EPOCH $END_EPOCH" > "docs/evidencias/graficos/.window-$PHASE"
echo "=== [$PHASE] janela: $START_EPOCH .. $END_EPOCH ($((END_EPOCH-START_EPOCH))s) ==="

# Descobre a porta host publicada do Prometheus (pode ter sido remapeada pelo override
# para evitar conflito com outros projetos). O plotter consulta por aí.
PROM_PORT="$(docker port cotacao-prometheus 9090 2>/dev/null | head -1 | sed 's/.*://')"
export PROM_URL="http://localhost:${PROM_PORT:-9090}"
echo "=== [$PHASE] consultando Prometheus em $PROM_URL ==="

echo "=== [$PHASE] gerando PNGs ==="
"$PY" scripts/plot_evidence.py "$PHASE" "$START_EPOCH" "$END_EPOCH"
echo "=== [$PHASE] pronto ==="
