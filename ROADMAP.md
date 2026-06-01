# Roadmap — Assistente de voz ADS-B + HA + LLM

Plano para evoluir o `flight-api` de um leitor de voos fixo para um assistente
mais rico, usando LLM. Documento vivo — retomar quando quiser.

## Onde estamos hoje (base já pronta)

- `flight-api` (Go) responde **"que voo é esse?"** via Home Assistant → Google Home.
- TTS **Piper local** com voz **pt_BR-faber-medium** (sem rate-limit).
- Modelo da "janela" por **proximidade** (≤4 km, à frente, não-decolando) — v0.3.4.
- HA roda como **VM no Proxmox** (pve01), estável.
- Dados: `aircraft.json` (readsb) + rota via **adsbdb.com**.
- HA já tem o pipeline de voz local instalado: **Whisper (STT) + Piper (TTS) + openWakeWord** (hoje só o Piper é usado).

## Cérebro do LLM: **OpenRouter**

API compatível com OpenAI, uma chave para vários modelos (Claude, GPT, Llama,
Gemini). Paga por uso, troca de modelo numa linha. Endpoint:
`POST https://openrouter.ai/api/v1/chat/completions` (Bearer key).
Modelo inicial sugerido: classe **Claude Haiku** (rápido, barato, bom PT-BR).

## Ordem de construção

### Tier 1 — Detalhes ricos via LLM  ⭐ começar aqui
A resposta de voz vira uma **narração do LLM**, não mais um template fixo.

Fluxo:
1. `flight-api` pega o voo (window/auto, como hoje).
2. **Enriquece**: airframe via **planespotters.net** (idade, operador, foto, nome),
   rota via adsbdb (já temos), fatos do tipo (o LLM já sabe).
3. Chama o **OpenRouter** com prompt PT-BR ("narre curto e natural pra voz, com
   1 fato interessante").
4. Devolve a narração → Piper → casa.

Detalhes técnicos:
- **Cache por matrícula** (mesmo avião não re-consulta LLM/planespotters).
- **Latência**: LLM adiciona ~1-3 s; usar modelo rápido + cache.
- Mantém o gatilho atual do Google (sem hardware novo).
- Provável: novo endpoint tipo `/narrate` ou flag `?narrate=1`; o announcer passa a usá-lo.
- Env novos: `OPENROUTER_KEY`, `OPENROUTER_MODEL`, talvez `NARRATION_LENGTH`.

### Tier 2 — Alertas inteligentes
- Watcher em background: quando algo **interessante** entra na janela (tipo raro,
  militar, livery especial, long-haul), o LLM decide se vale e cria o alerta.
- Detecção: regras (faixas de hex militar, tipos raros) + julgamento do LLM.
- Saída: push (ntfy) e/ou TTS na casa.
- Reaproveita o enriquecimento + LLM do Tier 1.

### Tier 3 — Assistente conversacional
- Agente LLM no **HA Assist** (conversation agent apontando pro OpenRouter),
  com o `flight-api` exposto como **ferramenta** (tool calling).
- Permite follow-ups, perguntas livres, e controle geral da casa.
- **Pré-requisito de hardware:** um **satélite de voz** (Home Assistant Voice PE,
  Atom Echo, ~R$80–300) ou o app do HA. O Google Home é rígido demais pra
  conversa livre roteada pro HA.

## Decisões pendentes (pra começar o Tier 1)

- [ ] Chave do **OpenRouter** (criar em openrouter.ai/keys, pôr crédito) → guardar
      no `~/flight-api/.env` do Pi.
- [ ] Tamanho da resposta: **curta** (1 frase) vs **detalhada** (2-3 frases).
- [ ] Modelo inicial (sugestão: Claude Haiku via OpenRouter).

## Ideias futuras (não priorizadas)

- 📸 Reviver a câmera (Pi `.33`): foto quando o avião entra na janela + modelo de
  **visão** descreve/confirma (resgata a ideia original do JetPhotos).
- 📊 Skills ADS-B: "quantos aviões agora?", "o mais distante?", "estatísticas do dia".
- 🌍 Limpar nomes de aeroporto longos do adsbdb ("Arnavutköy, Istanbul" → "Istambul").
- ⏰ Agendar backup da stack ADS-B (Pi `.33`).
