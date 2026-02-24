# Project: go-voice

go-voice is an AI-powered phone call agent designed for booking meetings. The system is built using Go language.

## Overview

This service initiates outbound calls using Twilio, connects calls to a websocket relay, saves transcript turns in an SQLite database, and leverages OpenAI functionality to:
- Generate the agent's next spoken reply during calls
- Grade the completed call transcript

The repository provides HTTP routes for various functionalities, including call creation, serving TwiML, managing the Twilio websocket relay, and grading transcripts.

### References
- For more details, check out [this resource](oaicite:0).

---

## Features

- **Outbound Call Creation:** Utilize Twilio `/Calls.json` API (`/call`)
- **TwiML Voice Response Endpoint:** Connects to a websocket relay (`/relay`)
- **Real-time Conversation Relay:** uses `gorilla/websocket`
- **Transcript Storage:** Implemented in SQLite (`modernc.org/sqlite`) with turn-by-turn records
- **AI-generated Next Reply:** Based on recent transcript context using OpenAI Responses API (`gpt-5.2`)
- **Fallback Deterministic Scheduling Flow:** Implemented as a backup if AI reply generation fails
- **Call Grading Endpoint:** Assessing completed calls using OpenAI structured JSON output

---

## How It Works

1. Initiate an outbound call by sending a POST request to `/call` with a phone number (in E.164 format).
2. Twilio processes the call and directs it to the app's `/voice` endpoint.
3. `/voice` returns TwiML to connect Twilio to `/relay` using a websocket.
4. The websocket relay captures conversation events (`setup`, `prompt`) and stores transcript turns in SQLite.
5. For each caller utterance:
   - If `OPENAI_API_KEY` is available, the app requests the next spoken line from OpenAI.
   - If this fails, it resorts to a predefined slot offering/confirmation flow.
6. To grade a call, use the `/grade?session_id=...` endpoint.

---

## Endpoints

### **POST /call**
Create an outbound call.

**Auth (optional):**
- When `API_TOKEN` exists, include:
  - `Authorization: Bearer <API_TOKEN>`

**Request Body**
```json
{
  "to": "+15551234567"
}
```

**Response**
```json
{
  "ok": true,
  "sessionID": "CAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

The `sessionID` corresponds to the Twilio Call SID returned by the create-call API.

- **POST /voice:** Returns TwiML to establish the connection between Twilio and the websocket relay for voice interaction.

- **GET /relay (WebSocket):** Websocket endpoint for Twilio Conversation Relay messages such as setup and prompt.

- **GET /grade?session_id=<id>:** Grades a completed transcript using OpenAI.

### **Environment Variables**

- **Required:**
  - TWILIO_ACCOUNT_SID
  - TWILIO_AUTH_TOKEN
  - TWILIO_FROM_NUMBER
  - PUBLIC_BASE_URL
  - OPENAI_API_KEY

- **Optional:**
  - API_TOKEN
  - PORT

---

## Running Locally

### **Prerequisites**

- Installed Go
- Twilio account with a phone number
- Public HTTPS URL (required for Twilio webhooks/relay)

1. Set up environment variables.
2. Run the server using `go run .`.

Example Requests:

- Create a call:
  ```
  curl -X POST http://localhost:8080/call \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $API_TOKEN" \
    -d '{"to":"+15551234567"}'
  ```

- Grade a call transcript:
  ```
  curl "http://localhost:8080/grade?session_id=CAxxxxxxxxxxxxxxxxxxxxxxxx" \
  -H "Authorization: Bearer $API_TOKEN"
  ```

---

## Data Storage

Transcript turns are stored in an SQLite table named `transcript_turns`, containing fields such as `session_id`, `call_sid`, `ts_utc`, `speaker`, and `text`.

---

## Security Considerations

Before deploying in a production environment, ensure:

- Restrict websocket origins
- Validate/sign Twilio requests
- Avoid logging sensitive data
- Enhance endpoint security
- Rotate API keys
- Implement rate limiting and request timeouts as necessary

---

## Deployment

This repository includes a `Dockerfile` and `fly.toml` for containerized deployment, intended for use with Fly.io.

---

## Project Status

This is an early prototype/MVP showcasing AI-assisted appointment booking via phone. The current repository is compact and focuses on demonstrating the end-to-end call flow.

---

## License

Please note that there is no license information available in the repository.