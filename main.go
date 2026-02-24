package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

type Config struct {
	AccountSID    string
	AuthToken     string
	FromNumber    string
	PublicBaseURL string // e.g. https://your-app.fly.dev
	APIToken      string // optional simple auth for /call
	OpenAIToken   string
}

func mustEnv(k string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		log.Fatalf("missing env var %s", k)
	}
	return v
}

func loadConfig() Config {
	cfg := Config{
		AccountSID:    mustEnv("TWILIO_ACCOUNT_SID"),
		AuthToken:     mustEnv("TWILIO_AUTH_TOKEN"),
		FromNumber:    mustEnv("TWILIO_FROM_NUMBER"),
		PublicBaseURL: mustEnv("PUBLIC_BASE_URL"),
		APIToken:      strings.TrimSpace(os.Getenv("API_TOKEN")),
		OpenAIToken:   mustEnv("OPENAI_API_KEY"),
	}
	return cfg
}

// ---------- TwiML XML structs (minimal) ----------

type VoiceResponse struct {
	XMLName xml.Name `xml:"Response"`
	Connect *Connect `xml:"Connect,omitempty"`
}

type Connect struct {
	ConversationRelay *ConversationRelay `xml:"ConversationRelay,omitempty"`
}

type ConversationRelay struct {
	URL                          string `xml:"url,attr"` // must be wss://
	WelcomeGreeting              string `xml:"welcomeGreeting,attr,omitempty"`
	WelcomeGreetingInterruptible string `xml:"welcomeGreetingInterruptible,attr,omitempty"` // none|dtmf|speech|any
	Language                     string `xml:"language,attr,omitempty"`
}

// ---------- ConversationRelay WS message types (subset) ----------

type SetupMsg struct {
	Type      string `json:"type"` // "setup"
	SessionID string `json:"sessionId"`
	CallSid   string `json:"callSid"`
}

type PromptMsg struct {
	Type        string `json:"type"` // "prompt"
	VoicePrompt string `json:"voicePrompt"`
	Last        bool   `json:"last"`
}

type TextMsg struct {
	Type  string `json:"type"` // "text"
	Token string `json:"token"`
	Last  bool   `json:"last"`
}

type EndMsg struct {
	Type        string `json:"type"` // "end"
	HandoffData string `json:"handoffData,omitempty"`
}

// ---------- Transcript ----------

type SQLiteTranscripts struct {
	db         *sql.DB
	insertStmt *sql.Stmt
}

func NewSQLiteTranscripts(path string) (*SQLiteTranscripts, error) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, err
	}

	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}

	schema := `
CREATE TABLE IF NOT EXISTS transcript_turns (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  call_sid   TEXT,
  ts_utc     TEXT NOT NULL,
  speaker    TEXT NOT NULL,
  text       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_transcript_session
  ON transcript_turns(session_id, id);
`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}

	stmt, err := db.Prepare(`
INSERT INTO transcript_turns (session_id, call_sid, ts_utc, speaker, text)
VALUES (?, ?, ?, ?, ?)
`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &SQLiteTranscripts{db: db, insertStmt: stmt}, nil
}

func (s *SQLiteTranscripts) AddTurn(sessionID, callSid, speaker, text string) {
	text = strings.TrimSpace(text)
	if text == "" || sessionID == "" {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.insertStmt.Exec(sessionID, callSid, ts, speaker, text)
}

func (s *SQLiteTranscripts) Close() error {
	if s.insertStmt != nil {
		_ = s.insertStmt.Close()
	}
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// SQL Query helper

type Turn struct {
	ID      int64
	TSUTC   string
	Speaker string
	Text    string
}

func loadTranscript(db *sql.DB, sessionID string, limit int) ([]Turn, error) {
	rows, err := db.Query(`
SELECT id, ts_utc, speaker, text
FROM transcript_turns
WHERE session_id = ?
ORDER BY id ASC
LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		var t Turn
		if err := rows.Scan(&t.ID, &t.TSUTC, &t.Speaker, &t.Text); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func formatDialogue(turns []Turn) string {
	var b strings.Builder
	for _, t := range turns {
		fmt.Fprintf(&b, "%s: %s\n", strings.ToUpper(t.Speaker), strings.TrimSpace(t.Text))
	}
	return b.String()
}

// ---------- Simple scheduling state ----------

type Stage int

const (
	Ask Stage = iota
	Offer
	Confirm
)

type SessionState struct {
	Stage        Stage
	OfferedSlots []time.Time
	Selected     *time.Time
}

type Server struct {
	cfg Config

	mu          sync.Mutex
	sessions    map[string]*SessionState
	transcripts *SQLiteTranscripts
}

func NewServer(cfg Config) *Server {
	return &Server{
		cfg:      cfg,
		sessions: make(map[string]*SessionState),
	}
}

func isYes(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "yes") || strings.Contains(s, "yeah") || strings.Contains(s, "sure") || strings.Contains(s, "okay") || strings.Contains(s, "ok")
}
func isNo(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "no") || strings.Contains(s, "not") || strings.Contains(s, "later")
}

var optRe = regexp.MustCompile(`\b(option\s*)?([1-3])\b|\b(first|second|third)\b`)

func parseOption(s string) (int, bool) {
	s = strings.ToLower(s)
	m := optRe.FindStringSubmatch(s)
	if len(m) == 0 {
		return 0, false
	}
	switch m[2] {
	case "1":
		return 1, true
	case "2":
		return 2, true
	case "3":
		return 3, true
	}
	switch m[3] {
	case "first":
		return 1, true
	case "second":
		return 2, true
	case "third":
		return 3, true
	}
	return 0, false
}

func nextSlots(now time.Time, count int) []time.Time {
	loc := now.Location()
	var out []time.Time
	t := now
	for len(out) < count {
		t = t.Add(24 * time.Hour)
		if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
			continue
		}
		for _, hr := range []int{10, 13, 16} {
			out = append(out, time.Date(t.Year(), t.Month(), t.Day(), hr, 0, 0, 0, loc))
			if len(out) >= count {
				break
			}
		}
	}
	return out
}
func fmtSlot(t time.Time) string { return t.Format("Monday at 3:04 PM") }

// ---------- Twilio: create outbound call ----------

type TwilioCreateCallResponse struct {
	SID    string `json:"sid"`
	Status string `json:"status"`
	To     string `json:"to"`
	From   string `json:"from"`
}

func (s *Server) createCall(to string) (string, error) {
	endpoint := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Calls.json", s.cfg.AccountSID)

	form := url.Values{}
	form.Set("To", to)
	form.Set("From", s.cfg.FromNumber)
	form.Set("Url", s.cfg.PublicBaseURL+"/voice")

	req, err := http.NewRequest("POST", endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	cred := base64.StdEncoding.EncodeToString([]byte(s.cfg.AccountSID + ":" + s.cfg.AuthToken))
	req.Header.Set("Authorization", "Basic "+cred)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// NOTE: Decode must happen AFTER we verify status code, otherwise body may be partially consumed.
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("twilio create call failed: %s\n%s", resp.Status, string(raw))
	}

	var twilioResp TwilioCreateCallResponse
	if err := json.Unmarshal(raw, &twilioResp); err != nil {
		return "", err
	}

	return twilioResp.SID, nil
}

// ---------- OpenAI grader (unchanged) ----------

type GradeResult struct {
	OverallScore int  `json:"overall_score"` // 0-100
	Pass         bool `json:"pass"`
	Scores       struct {
		Politeness              int `json:"politeness"`
		Clarity                 int `json:"clarity"`
		SchedulingEffectiveness int `json:"scheduling_effectiveness"`
		ComplianceDisclosure    int `json:"compliance_disclosure"`
	} `json:"scores"`
	Issues     []string `json:"issues"`
	Highlights []string `json:"highlights"`
	Summary    string   `json:"summary"`
}

func gradeWithOpenAI(apiKey string, transcriptText string) (GradeResult, error) {
	schema := map[string]any{
		"name":   "call_grade",
		"strict": true,
		"schema": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"overall_score": map[string]any{"type": "integer", "minimum": 0, "maximum": 100},
				"pass":          map[string]any{"type": "boolean"},
				"scores": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"politeness":               map[string]any{"type": "integer", "minimum": 0, "maximum": 10},
						"clarity":                  map[string]any{"type": "integer", "minimum": 0, "maximum": 10},
						"scheduling_effectiveness": map[string]any{"type": "integer", "minimum": 0, "maximum": 10},
						"compliance_disclosure":    map[string]any{"type": "integer", "minimum": 0, "maximum": 10},
					},
					"required": []string{"politeness", "clarity", "scheduling_effectiveness", "compliance_disclosure"},
				},
				"issues":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"highlights": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"summary":    map[string]any{"type": "string"},
			},
			"required": []string{"overall_score", "pass", "scores", "issues", "highlights", "summary"},
		},
	}

	body := map[string]any{
		"model": "gpt-5.2",
		"input": []map[string]any{
			{
				"role": "system",
				"content": []map[string]any{
					{"type": "text", "text": "You are grading an AI appointment-scheduling phone call. Grade quality, clarity, scheduling success, and whether the bot disclosed it is automated/AI. Be strict and concrete."},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "Here is the call transcript:\n\n" + transcriptText},
				},
			},
		},
		"text": map[string]any{
			"format": map[string]any{
				"type":        "json_schema",
				"json_schema": schema,
			},
		},
	}

	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/responses", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return GradeResult{}, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return GradeResult{}, fmt.Errorf("openai error: %s: %s", resp.Status, string(raw))
	}

	var r struct {
		Output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return GradeResult{}, err
	}

	var jsonText string
	for _, o := range r.Output {
		for _, c := range o.Content {
			if c.Type == "output_text" && c.Text != "" {
				jsonText = c.Text
				break
			}
		}
		if jsonText != "" {
			break
		}
	}
	if jsonText == "" {
		return GradeResult{}, fmt.Errorf("no output_text found in response")
	}

	var gr GradeResult
	if err := json.Unmarshal([]byte(jsonText), &gr); err != nil {
		return GradeResult{}, fmt.Errorf("failed to parse grader JSON: %w; text=%q", err, jsonText)
	}
	return gr, nil
}

// ---------- OpenAI agent response handler (ADDED) ----------

const openAIResponsesURL = "https://api.openai.com/v1/responses"

type responsesCreateRequest struct {
	Model           string        `json:"model"`
	Reasoning       *reasoningCfg `json:"reasoning,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens,omitempty"`
	Input           any           `json:"input"`
}

type reasoningCfg struct {
	Effort string `json:"effort"` // "none" | "low" | "medium" | "high" | "xhigh"
}

type responsesCreateResponse struct {
	Output []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// aiNextReply builds a "spoken" next message based on recent transcript.
// It returns ONLY the bot's next line to speak.
func (s *Server) aiNextReply(sessionID string) (string, error) {
	if s.cfg.OpenAIToken == "" {
		return "", errors.New("missing OpenAI token")
	}
	if s.transcripts == nil || s.transcripts.db == nil {
		return "", errors.New("transcript store not initialized")
	}

	turns, err := loadTranscript(s.transcripts.db, sessionID, 200)
	if err != nil {
		return "", err
	}
	if len(turns) == 0 {
		return "", errors.New("no transcript")
	}
	dialogue := formatDialogue(turns)

	// IMPORTANT: Keep this very "voice friendly" and short.
	developerPrompt := "You are an automated appointment-scheduling voice agent on a live phone call. " +
		"You are Mark an automated assistant for <company-name> scheduling appointments with <user-name>" +
		"Respond naturally and concisely. Short sentences. No markdown. No bullets. " +
		"Ask at most ONE clarifying question. If you propose times, give up to 3 options. " +
		"Do not mention you used a transcript or any internal process. " +
		"Return ONLY the next thing the agent should say."

	reqBody := map[string]any{
		"model": "gpt-5.2",
		"reasoning": map[string]any{
			"effort": "medium",
		},
		"max_output_tokens": 180,
		"input": []map[string]any{
			{
				"role": "developer",
				"content": []map[string]any{
					{"type": "input_text", "text": developerPrompt},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "Here is the call transcript so far:\n\n" + dialogue + "\n\nGenerate the agent's NEXT spoken reply only."},
				},
			},
		},
	}

	b, _ := json.Marshal(reqBody)
	httpReq, err := http.NewRequest("POST", openAIResponsesURL, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.cfg.OpenAIToken)

	client := &http.Client{Timeout: 20 * time.Second}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer httpResp.Body.Close()

	raw, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		var e responsesCreateResponse
		_ = json.Unmarshal(raw, &e)
		if e.Error != nil && e.Error.Message != "" {
			return "", fmt.Errorf("openai error (%d): %s", httpResp.StatusCode, e.Error.Message)
		}
		return "", fmt.Errorf("openai http error (%d): %s", httpResp.StatusCode, string(raw))
	}

	var resp responsesCreateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return "", fmt.Errorf("openai error: %s", resp.Error.Message)
	}

	var out strings.Builder
	for _, item := range resp.Output {
		if item.Type != "message" || item.Role != "assistant" {
			continue
		}
		for _, c := range item.Content {
			// Some responses return "output_text"; this handler mirrors your grader extraction.
			if c.Type == "output_text" && strings.TrimSpace(c.Text) != "" {
				if out.Len() > 0 {
					out.WriteString("\n")
				}
				out.WriteString(c.Text)
			}
		}
	}

	reply := strings.TrimSpace(out.String())
	if reply == "" {
		return "", errors.New("empty model reply")
	}

	// Optional safety cleanup: remove surrounding quotes if the model adds them.
	reply = strings.Trim(reply, "\"")
	reply = strings.TrimSpace(reply)

	return reply, nil
}

// ---------- Handlers ----------

func (s *Server) callHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.APIToken != "" {
		if r.Header.Get("Authorization") != "Bearer "+s.cfg.APIToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var in struct {
		To string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	in.To = strings.TrimSpace(in.To)
	if !strings.HasPrefix(in.To, "+") {
		http.Error(w, "to must be E.164 like +15551234567", http.StatusBadRequest)
		return
	}
	sessionID, err := s.createCall(in.To)
	if err != nil {
		log.Println("createCall error:", err)
		http.Error(w, "failed to create call", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "sessionID": sessionID})
}

func (s *Server) voiceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")

	vr := VoiceResponse{
		Connect: &Connect{
			ConversationRelay: &ConversationRelay{
				URL:                          strings.Replace(s.cfg.PublicBaseURL, "https://", "wss://", 1) + "/relay",
				WelcomeGreeting:              "Hi! this is Mark an automated assistant with ABQ-IT. Would you like to schedule an appointment for an IT consultation?",
				WelcomeGreetingInterruptible: "any",
				Language:                     "en-US",
			},
		},
	}
	_ = xml.NewEncoder(w).Encode(vr)
}

func (s *Server) gradeHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, "missing session_id", http.StatusBadRequest)
		return
	}
	if s.cfg.APIToken != "" {
		if r.Header.Get("Authorization") != "Bearer "+s.cfg.APIToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	turns, err := loadTranscript(s.transcripts.db, sessionID, 500)
	if err != nil {
		http.Error(w, "failed to load transcript", http.StatusInternalServerError)
		return
	}
	if len(turns) == 0 {
		http.Error(w, "no transcript found for session_id", http.StatusNotFound)
		return
	}

	dialogue := formatDialogue(turns)

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		http.Error(w, "missing OPENAI_API_KEY", http.StatusInternalServerError)
		return
	}

	grade, err := gradeWithOpenAI(apiKey, dialogue)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(grade)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (s *Server) relayHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "ws upgrade failed", http.StatusBadRequest)
		log.Println("ws upgrade failed")
		return
	}
	defer conn.Close()

	var sessionID string
	var callSid string

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}

		switch env.Type {
		case "setup":
			var m SetupMsg
			_ = json.Unmarshal(data, &m)
			sessionID = m.SessionID
			callSid = m.CallSid
			log.Println("call sessionID: " + callSid)

			s.transcripts.AddTurn(sessionID, callSid, "system", "setup received")

			s.mu.Lock()
			s.sessions[sessionID] = &SessionState{Stage: Ask}
			s.mu.Unlock()

		case "prompt":
			var m PromptMsg
			_ = json.Unmarshal(data, &m)
			if !m.Last || sessionID == "" {
				continue
			}
			s.transcripts.AddTurn(sessionID, callSid, "caller", m.VoicePrompt)
			s.handlePrompt(conn, sessionID, callSid, m.VoicePrompt)
		}
	}
}

func (s *Server) say(conn *websocket.Conn, sessionID, callSid, text string) {
	s.transcripts.AddTurn(sessionID, callSid, "bot", text)
	_ = conn.WriteJSON(TextMsg{Type: "text", Token: text, Last: true})
}
func (s *Server) end(conn *websocket.Conn, handoff string) {
	_ = conn.WriteJSON(EndMsg{Type: "end", HandoffData: handoff})
}

func (s *Server) handlePrompt(conn *websocket.Conn, sessionID, callSid, utter string) {
	utter = strings.TrimSpace(utter)

	// --- NEW: If OpenAI is configured, let it generate the next spoken line from transcript.
	// If it errors, fall back to your existing state machine behavior.
	if s.cfg.OpenAIToken != "" {
		if reply, err := s.aiNextReply(sessionID); err == nil && strings.TrimSpace(reply) != "" {
			s.say(conn, sessionID, callSid, reply)
			return
		} else if err != nil {
			log.Println("aiNextReply error (falling back):", err)
		}
	}

	// --- Existing deterministic scheduling logic fallback ---
	s.mu.Lock()
	st := s.sessions[sessionID]
	s.mu.Unlock()
	if st == nil {
		return
	}

	switch st.Stage {
	case Ask:
		if isYes(utter) {
			st.OfferedSlots = nextSlots(time.Now(), 3)
			st.Stage = Offer
			s.say(conn, sessionID, callSid, fmt.Sprintf(
				"Great. I have option 1: %s. Option 2: %s. Option 3: %s. Which do you prefer?",
				fmtSlot(st.OfferedSlots[0]),
				fmtSlot(st.OfferedSlots[1]),
				fmtSlot(st.OfferedSlots[2]),
			))
			return
		}
		if isNo(utter) {
			s.say(conn, sessionID, callSid, "No problem. Goodbye.")
			s.end(conn, `{"result":"declined"}`)
			return
		}
		s.say(conn, sessionID, callSid, "Sorry, did you want to schedule an appointment? Please say yes or no.")
		return

	case Offer:
		if n, ok := parseOption(utter); ok && n >= 1 && n <= len(st.OfferedSlots) {
			chosen := st.OfferedSlots[n-1]
			st.Selected = &chosen
			st.Stage = Confirm
			s.say(conn, sessionID, callSid, fmt.Sprintf("Okay. Confirm booking %s? Please say yes or no.", fmtSlot(chosen)))
			return
		}
		s.say(conn, sessionID, callSid, "Please say option 1, option 2, or option 3.")
		return

	case Confirm:
		if st.Selected == nil {
			st.Stage = Ask
			s.say(conn, sessionID, callSid, "Let’s start over. Would you like to schedule an appointment?")
			return
		}
		if isYes(utter) {
			s.say(conn, sessionID, callSid, fmt.Sprintf("You’re booked for %s. Goodbye.", fmtSlot(*st.Selected)))
			s.end(conn, `{"result":"booked"}`)
			return
		}
		if isNo(utter) {
			st.Stage = Offer
			st.OfferedSlots = nextSlots(time.Now(), 3)
			s.say(conn, sessionID, callSid, fmt.Sprintf(
				"Okay. New options are: 1: %s. 2: %s. 3: %s. Which works?",
				fmtSlot(st.OfferedSlots[0]),
				fmtSlot(st.OfferedSlots[1]),
				fmtSlot(st.OfferedSlots[2]),
			))
			return
		}
		s.say(conn, sessionID, callSid, "Please say yes to confirm, or no to choose another time.")
	}
}

func main() {
	cfg := loadConfig()
	s := NewServer(cfg)
	tstore, err := NewSQLiteTranscripts("./transcripts.sqlite")
	if err != nil {
		log.Fatal(err)
	}
	defer tstore.Close()
	s.transcripts = tstore

	mux := http.NewServeMux()
	mux.HandleFunc("/call", s.callHandler)
	mux.HandleFunc("/voice", s.voiceHandler)
	mux.HandleFunc("/relay", s.relayHandler)
	mux.HandleFunc("/grade", s.gradeHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Println("listening on :" + port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, mux))
}
