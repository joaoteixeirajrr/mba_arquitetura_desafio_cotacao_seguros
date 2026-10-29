# Prumo Cota — resiliência da fronteira com as seguradoras (README do processo)

Extensão da plataforma de cotação do starter: fecha os buracos do "antes" (agregação em série, sem
timeout, tudo-ou-nada) com **circuit breaker + timeout por parceira**, **cache tenant-scoped** e
**fallback**, tudo instrumentado. O enunciado original está em
[`docs/enunciado.md`](docs/enunciado.md).

## 1. SAD e evidências

- **SAD (Entrega 1):** [`docs/sad.md`](docs/sad.md) — 8 seções, 4 diagramas C4 em Mermaid.
- **Evidências:** [`docs/evidencias/`](docs/evidencias/)
  - [`antes-reproduce.txt`](docs/evidencias/antes-reproduce.txt) / [`depois-reproduce.txt`](docs/evidencias/depois-reproduce.txt) — relatório `make reproduce`, mesma carga (10 baseline + 200 com 50 em voo).
  - [`make-test.txt`](docs/evidencias/make-test.txt) — suite inteira verde (`go test -count=1 ./...`).
  - [`promql.md`](docs/evidencias/promql.md) — consultas PromQL + valores medidos (estado do breaker, hit rate, p95, latência por parceira), com janela declarada.
  - [`trace-short-circuited.json`](docs/evidencias/trace-short-circuited.json) — requisição com breaker aberto: a parceira curto-circuitada **não tem span de saída**.
  - [`trace-cache-served.json`](docs/evidencias/trace-cache-served.json) — resposta de cache: só o span de servidor, nenhum span de parceira.

Resumo antes → depois (mesma carga, mesma máquina, ambiente zerado):

| | antes | depois |
|---|---|---|
| sucesso baseline / carga | 50% / 60% | **100% / 100%** |
| p95 carga | 8,01 s | **2,01 s** |
| p50 carga | 6,12 s | **11 ms** (cache quente) |
| vazão carga | 8,0 req/s | **95,8 req/s** |

## 2. Como subir e reproduzir

Pré-requisitos: Docker + Docker Compose e Go 1.25 (para `make test`/`make smoke`).

```bash
make down && make reproduce   # sobe o ambiente do zero e roda a carga padrão (o "depois")
make test                     # suite inteira, incluindo os testes que já vinham
make smoke                    # prova determinística de factibilidade do breaker (sem Docker)
```

Observabilidade com o ambiente de pé: Jaeger em <http://localhost:16686>, Prometheus em
<http://localhost:9090>. As consultas PromQL estão em [`docs/evidencias/promql.md`](docs/evidencias/promql.md).
Para reproduzir as marcações de trace, derrube as parceiras via a carga e abra o trace mais longo /
um trace servido de cache.

## 3. O que foi implementado e o que ficou como proposta

**Implementado (código + teste determinístico):**

- Circuit breaker per-parceira com três estados — `internal/resilience/breaker.go`.
- Timeout de 2 s por chamada — `internal/partner/client.go` (`WithTimeout`); o estouro conta como falha.
- Cache tenant-scoped sobre Redis — `internal/cache/`.
- Agregação **paralela** (bônus) + fallback (parcial / cache vencido marcado) — `internal/quotation/resilient.go`.
- Contrato `Response` estendido com `degraded` — `internal/quotation/request.go`.
- 3 métricas de negócio + 2 marcações de trace — `internal/platform/metrics.go`.

**Proposta (no SAD, fora do código):** hospedagem híbrida (seção 5), failover de região (seção 7, cenário
c) e o fechamento dos quatro limites conhecidos (LIM-01 a LIM-04, seção 4): limite de concorrência por
parceira, breaker distribuído entre réplicas, invalidação de cache por evento e bulkhead/quota por
corretora.

## 4. Métricas e atributos criados

Meter `quotation-api` (nomes procuráveis no repositório, em `internal/platform/metrics.go`):

| Métrica | Tipo | Atributos |
|---|---|---|
| `quotation_breaker_transitions_total` | counter | `partner`, `from`, `to` |
| `quotation_breaker_state` | observable gauge (0 closed / 1 open / 2 half_open) | `partner` |
| `quotation_cache_events_total` | counter | `tenant_id`, `result` (`hit`/`miss`) |
| `quotation_partner_request_duration_seconds` | histogram | `partner`, `outcome` (`success`/`failure`) |

Marcações de trace (eventos no span ativo): `breaker.short_circuited` (attr `partner`), `cache.served`
(attr `stale`). **Latência por parceira** reaproveita também a métrica genérica de borda HTTP
(`http_client_request_duration_seconds`, attr `server_address`) — ver `promql.md`.

**Atributos:** apenas `tenant_id` / `partner` / `from` / `to` / `result` / `outcome`. **CPF, placa e
`quote_id` nunca entram** em span ou métrica (dado pessoal + cardinalidade sem teto), garantido pelo
teste `TestNoPersonalDataInAttributeKeys` e pela chave de cache hasheada.

## 5. O que eu faria diferente com mais tempo

Fecharia LIM-02 primeiro (breaker distribuído via Redis), porque é o que mais atrapalha ao escalar
réplicas; depois um limite de concorrência por parceira (LIM-01), que é a causa de a `partner-degrading`
ainda bater no timeout sob pico. Também mediria o hit rate real de produção para validar o pressuposto
PS-01, já que ele é a variável que sustenta a planilha de custos.

---

**O coração da solução.** Protegi a **fronteira com as seguradoras**: cada parceira ganhou um circuit
breaker próprio (abre em 5 falhas consecutivas, cooldown de 30 s, fecha com 2 sucessos) e um timeout de
2 s, de modo que uma parceira doente é **cortada** em vez de esperada — e, aberta, **não é chamada**,
economizando a consulta de R$ 0,04. Pus na frente um cache da cotação agregada, isolado por corretora
(`quote:v1:{tenant}:{coverage}:{sha256(risco)}`), que derruba a latência do caso repetido para
milissegundos. E troquei o "tudo-ou-nada" por um fallback que devolve a resposta parcial ou a cotação
anterior marcada, para que a corretora receba sempre alguma cotação. O custo é um contrato de resposta
mais rico (campo `degraded`) e quatro limites conscientes e declarados; o retorno é sucesso de 60% para
100%, p95 de 8 s para 2 s, e metade da conta de consulta que o cache pode poupar.
