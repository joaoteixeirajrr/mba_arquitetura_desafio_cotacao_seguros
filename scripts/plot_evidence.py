#!/usr/bin/env python3
"""Captura séries do Prometheus (API query_range) e gera os PNGs de evidência.

Cada figura traz o gráfico à esquerda e, à direita, a consulta PromQL e a janela
declarada — exatamente o que o enunciado pede ("cada um com a consulta e a janela
ao lado"). Os três gráficos de uma mesma execução ("antes" ou "depois") usam a
mesma escala de eixo Y por tipo, para que antes/depois sejam comparáveis.

Uso:
    plot_evidence.py <fase> <start_epoch> <end_epoch> [--prom URL] [--out DIR]

onde <fase> é "antes" ou "depois".
"""
import argparse
import os
import sys
from pathlib import Path

import matplotlib
matplotlib.use("Agg")
import matplotlib.dates as mdates
import matplotlib.pyplot as plt
import requests
from datetime import datetime, timezone

STEP = "5s"  # casa com o scrape_interval do Prometheus (deploy/prometheus/prometheus.yml)

# Escalas fixas por tipo de gráfico, para "antes" e "depois" ficarem na MESMA escala.
YLIM = {
    "breaker": (-0.15, 2.3),   # gauge 0/1/2
    "hitrate": (-0.03, 1.03),  # taxa 0..1
    "p95": (0, 11.0),          # segundos — cobre o pico sob carga do "antes" (~9,9s)
}

QUERIES = {
    "breaker": {
        "title": "Estado do breaker no tempo (0=closed, 1=open, 2=half_open)",
        "promql": 'quotation_breaker_state',
        "legend": lambda m: m.get("partner", "série"),
        "ylabel": "estado",
        "yticks": ([0, 1, 2], ["closed", "open", "half_open"]),
    },
    "hitrate": {
        "title": "Hit rate do cache (curva subindo conforme o cache aquece)",
        "promql": (
            'sum(rate(quotation_cache_events_total{result="hit"}[1m]))\n'
            '/\n'
            'sum(rate(quotation_cache_events_total[1m]))'
        ),
        "legend": lambda m: "hit rate",
        "ylabel": "hit rate",
        "yticks": None,
    },
    "p95": {
        "title": "p95 do POST /quotes (ponta, visão da corretora)",
        "promql": (
            'histogram_quantile(0.95, sum by (le) (rate(\n'
            '  http_server_request_duration_seconds_bucket{http_route="/quotes"}[1m])))'
        ),
        "legend": lambda m: "p95 /quotes",
        "ylabel": "segundos",
        "yticks": None,
    },
}


def query_range(prom, expr, start, end, step):
    r = requests.get(
        f"{prom}/api/v1/query_range",
        params={"query": expr, "start": start, "end": end, "step": step},
        timeout=30,
    )
    r.raise_for_status()
    data = r.json()
    if data.get("status") != "success":
        raise RuntimeError(f"Prometheus respondeu {data}")
    return data["data"]["result"]


def plot(kind, phase, start, end, prom, out_dir):
    spec = QUERIES[kind]
    expr = spec["promql"].replace("\n", " ").replace("  ", " ").strip()
    series = query_range(prom, expr, start, end, STEP)

    fig = plt.figure(figsize=(12, 4.2))
    gs = fig.add_gridspec(1, 2, width_ratios=[2.6, 1.0], wspace=0.08)
    ax = fig.add_subplot(gs[0])
    side = fig.add_subplot(gs[1])
    side.axis("off")

    plotted = False
    for s in series:
        metric = s.get("metric", {})
        xs, ys = [], []
        for ts, val in s["values"]:
            try:
                y = float(val)
            except ValueError:
                continue  # NaN
            if y != y:  # NaN
                continue
            xs.append(datetime.fromtimestamp(float(ts), tz=timezone.utc))
            ys.append(y)
        if not xs:
            continue
        plotted = True
        drawstyle = "steps-post" if kind == "breaker" else "default"
        ax.plot(xs, ys, drawstyle=drawstyle, marker="o", markersize=2.5,
                linewidth=1.6, label=spec["legend"](metric))

    ax.set_title(f"[{phase}] {spec['title']}", fontsize=11, loc="left")
    ax.set_ylabel(spec["ylabel"])
    ax.set_ylim(*YLIM[kind])
    # Fixa o eixo X na janela pedida mesmo sem dados, para os gráficos vazios
    # ("antes") exibirem o período correto em vez de 00:00:00.
    ax.set_xlim(datetime.fromtimestamp(start, tz=timezone.utc),
                datetime.fromtimestamp(end, tz=timezone.utc))
    if spec["yticks"]:
        ax.set_yticks(spec["yticks"][0])
        ax.set_yticklabels(spec["yticks"][1])
    ax.xaxis.set_major_formatter(mdates.DateFormatter("%H:%M:%S", tz=timezone.utc))
    ax.grid(True, alpha=0.3)
    fig.autofmt_xdate(rotation=0, ha="center")

    if plotted:
        ax.legend(loc="upper right", fontsize=8)
    else:
        ax.text(0.5, 0.5, "sem série neste período\n(métrica não existe nesta execução)",
                ha="center", va="center", transform=ax.transAxes,
                fontsize=10, color="#b00")

    start_iso = datetime.fromtimestamp(start, tz=timezone.utc).strftime("%H:%M:%S")
    end_iso = datetime.fromtimestamp(end, tz=timezone.utc).strftime("%H:%M:%S")
    box = (
        "CONSULTA (PromQL)\n"
        "────────────────\n"
        f"{spec['promql']}\n\n"
        "JANELA\n"
        "────────────────\n"
        f"{start_iso} → {end_iso} UTC\n"
        f"step {STEP}  (scrape 5s)\n"
        f"fonte: Prometheus\n"
        f"http://localhost:9090"
    )
    side.text(0.0, 1.0, box, ha="left", va="top", family="monospace",
              fontsize=8.0, transform=side.transAxes,
              bbox=dict(boxstyle="round,pad=0.5", fc="#f6f6f6", ec="#999"))

    out_dir.mkdir(parents=True, exist_ok=True)
    path = out_dir / f"{phase}-{kind}.png"
    fig.savefig(path, dpi=130, bbox_inches="tight")
    plt.close(fig)
    print(f"  escrito {path}  (séries plotadas: {len([s for s in series])}, com dados: {plotted})")
    return plotted


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("phase", choices=["antes", "depois"])
    ap.add_argument("start", type=float)
    ap.add_argument("end", type=float)
    ap.add_argument("--prom", default=os.environ.get("PROM_URL", "http://localhost:9090"))
    ap.add_argument("--out", default="docs/evidencias/graficos")
    args = ap.parse_args()

    out_dir = Path(args.out)
    print(f"[{args.phase}] janela {args.start:.0f}..{args.end:.0f} -> {out_dir}")
    ok = True
    for kind in ("breaker", "hitrate", "p95"):
        try:
            plot(kind, args.phase, args.start, args.end, args.prom, out_dir)
        except Exception as e:  # noqa: BLE001
            print(f"  ERRO em {kind}: {e}", file=sys.stderr)
            ok = False
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
