# flight-api

Micro-serviço em Go que responde **"qual avião está no céu agora"** usando dados
ADS-B locais (readsb/tar1090) e a integra com o **Home Assistant** para um
assistente de voz no Google Home: *"Ok Google, que voo é esse?"*

Faz parte do projeto de monitoramento de aeronaves perto do GRU (São Paulo).

## O que faz

1. **Servidor HTTP** (porta `8890`):
   - `GET /overhead` — aeronave mais próxima no ar (JSON + frase `speech` em PT-BR).
   - `GET /overhead?mode=window` — prioriza quem cruza o azimute da janela (167°).
   - `GET /healthz` — liveness: o processo está no ar (sempre 200).
   - `GET /ready` — readiness: **200** só quando o WebSocket com o HA está
     conectado; **503** caso contrário. Bom p/ monitorar a desconexão silenciosa
     do announcer (uptime-kuma etc.) — o `/healthz` não enxerga isso.
2. **Announcer** (opcional, se `HA_TOKEN` setado): abre um WebSocket com o Home
   Assistant, cria o gatilho `input_boolean.perguntar_voo`, escuta, e quando ele
   liga (via rotina do Google Home) calcula o voo e manda o HA **falar** a
   resposta no `media_player` configurado — depois desliga o gatilho.

A rota (origem/destino/companhia) é enriquecida pela API pública do
[adsbdb.com](https://www.adsbdb.com/).

## Configuração (variáveis de ambiente)

| Variável | Padrão | Descrição |
|---|---|---|
| `PORT` | `8890` | Porta do servidor HTTP |
| `HA_URL` | _(vazio)_ | URL do Home Assistant, ex. `http://192.168.0.55:8123` |
| `HA_TOKEN` | _(vazio)_ | Token de longa duração do HA. Vazio = sem voz, só HTTP |
| `HA_MEDIA_PLAYER` | `media_player.casa_toda` | Onde falar |
| `HA_TTS_ENTITY` | `tts.google_translate_en_com` | Motor de TTS |
| `HA_TTS_LANG` | `pt` | Idioma do TTS |
| `HA_TRIGGER_BOOLEAN` | `input_boolean.perguntar_voo` | Gatilho |
| `HA_TRIGGER_NAME` | `Perguntar Voo` | Nome do gatilho ao criá-lo |
| `ANNOUNCE_MODE` | `window` | `window` = só a janela; `nearest` = mais próximo no céu |
| `WINDOW_AZIMUTH` | `167` | Azimute da janela/varanda (graus) |
| `WINDOW_TOLERANCE` | `18` | ± graus aceitos ao redor do azimute |
| `MIN_ELEVATION_DEG` | `10` | Elevação mínima — corta tráfego baixo na pista |
| `OBSERVER_ALT_M` | `850` | Altitude do observador (m), para calcular a elevação |

No modo `window` o serviço só anuncia aeronaves dentro de `WINDOW_AZIMUTH ± WINDOW_TOLERANCE`
**e** acima de `MIN_ELEVATION_DEG` de elevação — assim evita anunciar aviões decolando,
baixos no horizonte. `GET /overhead?mode=nearest` ignora o filtro e pega o mais próximo.

O serviço lê o `aircraft.json` em `http://localhost:8080/data/aircraft.json`,
por isso roda com `network_mode: host` no mesmo host do tar1090.

## Rodando

```bash
cp .env.example .env      # preencha HA_TOKEN
docker compose pull       # ou: docker compose up -d --build
docker compose up -d
docker compose logs -f flight-api
```

## Build & publish (Docker Hub)

```bash
docker buildx build --platform linux/arm64 -t pagottoo/adsb-flight-api:latest --push .
```

## Integração com o Google Home

1. No HA, exponha `input_boolean.perguntar_voo` ao Google Assistant.
2. App Google Home → Rotinas → frase *"que voo é esse"* → ação: ligar **Perguntar Voo**.

## Desenvolvimento

```bash
go mod tidy
go build ./...
HA_TOKEN=... go run .
```
