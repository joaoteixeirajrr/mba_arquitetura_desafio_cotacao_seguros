# Evidências de métrica — consultas PromQL e marcações de trace

Cada gráfico do Prometheus se captura **durante** a execução (o Prometheus retém só 1h e `make down`
zera tudo). Abaixo vão a consulta, a janela e o valor medido nesta máquina em 2026-10-28, logo após
`make reproduce` seguido de 30 cotações distintas que forçaram o breaker a ciclar. Fonte: API do
Prometheus em `http://localhost:9090`.

## 1. Estado do breaker no tempo (degrau fechado → aberto → meio aberto → fechado)

```promql
quotation_breaker_state
```

Janela: instantânea (gauge). Legenda do valor: `0 = closed`, `1 = open`, `2 = half_open`, por parceira.

Medição (logo após a carga): `partner-flaky = 1` (open), `partner-degrading = 0` (voltou a closed).
O degrau completo está no contador de transições e nos logs da API:

```promql
quotation_breaker_transitions_total
```

Resultado medido (prova do ciclo inteiro, não só da existência):

```
partner-degrading closed -> open       = 1
partner-degrading open   -> half_open  = 1
partner-degrading half_open -> closed  = 1
partner-flaky     closed -> open       = 1
partner-flaky     open   -> half_open  = 1
partner-flaky     half_open -> open    = 1   (flapping: parceira de 40% nem morre nem recupera de vez)
```

Para desenhar o degrau de uma parceira no gráfico do Prometheus, use a série filtrada:

```promql
quotation_breaker_state{partner="partner-flaky"}
```

## 2. Hit rate do cache (a curva subindo conforme o cache aquece)

```promql
sum(rate(quotation_cache_events_total{result="hit"}[5m]))
/
sum(rate(quotation_cache_events_total[5m]))
```

Janela: `[5m]`. Hit rate medido ao fim da sessão: **≈ 0,30** (a janela de 5m já diluía a rajada de
misses das 30 cotações distintas que forçamos no breaker; durante o `make reproduce` puro, de 5
cotações repetidas, o acumulado chega a ~0,78 — ver contagem abaixo).

Contagem acumulada, que também prova o **isolamento por corretora** (atributo `tenant_id`):

```promql
quotation_cache_events_total
```

```
result=hit  tenant_id=corretora-a = 163
result=miss tenant_id=corretora-a = 77
result=hit  tenant_id=corretora-b = 39
result=miss tenant_id=corretora-b = 1
```

> O hit rate deste gráfico é propriedade da carga (5 cotações repetidas), não de produção. O número
> que sustenta a planilha da seção 8 do SAD sai do pressuposto de recotação, não daqui.

## 3. Latência p95 por parceira, com significado de domínio

```promql
histogram_quantile(0.95, sum by (partner, le) (rate(quotation_partner_request_duration_seconds_bucket[5m])))
```

Janela: `[5m]`. Medido: as três parceiras batem no teto do **timeout de 2s** sob concorrência (o p95
observado reflete o bucket do teto; é exatamente o efeito do ceiling que fizemos a `partner-degrading`
parar de afundar sem limite). Contagem por resultado:

```promql
quotation_partner_request_duration_seconds_count
```

```
partner-slow      success = 47
partner-flaky     success = 29   partner-flaky     failure = 18
partner-degrading success = 18   partner-degrading failure = 29
```

## 4. p95 do POST /quotes (ponta, do ponto de vista da corretora) — antes e depois

```promql
histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket{http_route="/quotes"}[5m])))
```

Janela: `[5m]`. Esta é a métrica genérica que já vinha pronta (reaproveitada, como permitido). O valor
do "antes" e do "depois" são duas capturas (o `make down` zera o Prometheus entre as sessões); a
comparação numérica está no relatório de texto:

| | antes (série) | depois (série) |
|---|---|---|
| p95 `POST /quotes` sob carga | 8,01 s (relatório loadgen) | 2,01 s (relatório loadgen) |
| p95 instantâneo pós-carga | — | 2,39 s (medido via a query acima) |

## Marcações de trace (Jaeger)

Export JSON em `docs/evidencias/`:

- **`trace-short-circuited.json`** — requisição com o breaker aberto. O trace tem **um único span de
  saída `HTTP POST`** (a parceira que foi chamada) e **dois eventos `breaker.short_circuited`**
  (`partner=partner-flaky`, `partner=partner-degrading`): as parceiras curto-circuitadas **não têm
  span de saída**, porque não foram chamadas. É a prova de que aberto = não chama.
- **`trace-cache-served.json`** — resposta servida de cache. O trace tem **só o span de servidor
  `POST /quotes`**, nenhum span de saída para parceira, e o evento `cache.served` (`stale=false`).

Janela no Jaeger: `Last Hour`, serviço `quotation-api`, operação `POST /quotes`.
