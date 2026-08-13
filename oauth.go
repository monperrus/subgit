package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// OAuth uses a GitHub OAuth App. Git itself cannot complete a browser login,
// so the browser flow creates a short-lived opaque password for Git HTTPS.
// The GitHub access token remains only in this process's memory.
type oauthState struct {
	returnTo string
	expires  time.Time
}

type pushSession struct {
	token   string
	expires time.Time
}

type OAuth struct {
	clientID     string
	clientSecret string
	publicURL    string
	mu           sync.Mutex
	states       map[string]oauthState
	sessions     map[string]pushSession
}

func newOAuth() *OAuth {
	return &OAuth{
		clientID: os.Getenv("GITHUB_OAUTH_CLIENT_ID"), clientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
		publicURL: strings.TrimSuffix(os.Getenv("SUBGIT_PUBLIC_URL"), "/"), states: map[string]oauthState{}, sessions: map[string]pushSession{},
	}
}

func (o *OAuth) enabled() bool { return o.clientID != "" && o.clientSecret != "" && o.publicURL != "" }

func randomID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (o *OAuth) begin(w http.ResponseWriter, r *http.Request) {
	if !o.enabled() {
		http.Error(w, "GitHub OAuth is not configured", http.StatusServiceUnavailable)
		return
	}
	returnTo := r.URL.Query().Get("return_to")
	if !strings.HasPrefix(returnTo, "/") || strings.HasPrefix(returnTo, "//") {
		http.Error(w, "return_to must be a local virtual-repository path", http.StatusBadRequest)
		return
	}
	// Validate only traversal here. The Git smart-HTTP handler performs the
	// authoritative identifier parsing; keeping this tolerant avoids URL-path
	// normalization differences between browser and Git clients.
	if strings.Contains(returnTo, "..") || !strings.HasSuffix(returnTo, ".git") {
		http.Error(w, "return_to must name a virtual .git repository", http.StatusBadRequest)
		return
	}
	state, err := randomID()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	o.mu.Lock()
	o.states[state] = oauthState{returnTo: returnTo, expires: time.Now().Add(10 * time.Minute)}
	o.mu.Unlock()
	v := url.Values{"client_id": {o.clientID}, "redirect_uri": {o.publicURL + "/auth/github/callback"}, "scope": {"repo workflow"}, "state": {state}}
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+v.Encode(), http.StatusFound)
}

func (o *OAuth) callback(w http.ResponseWriter, r *http.Request) {
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	o.mu.Lock()
	pending, ok := o.states[state]
	delete(o.states, state)
	o.mu.Unlock()
	if !ok || time.Now().After(pending.expires) || code == "" {
		http.Error(w, "OAuth state expired or invalid", http.StatusBadRequest)
		return
	}
	form := url.Values{"client_id": {o.clientID}, "client_secret": {o.clientSecret}, "code": {code}, "redirect_uri": {o.publicURL + "/auth/github/callback"}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "exchange GitHub authorization: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var result struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(b, &result)
	if resp.StatusCode != 200 || result.AccessToken == "" {
		http.Error(w, "GitHub authorization failed: "+result.Error+" "+result.ErrorDescription, 502)
		return
	}
	password, err := randomID()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	o.mu.Lock()
	o.sessions[password] = pushSession{token: result.AccessToken, expires: time.Now().Add(8 * time.Hour)}
	o.mu.Unlock()
	remote := strings.TrimPrefix(o.publicURL+pending.returnTo, "https://")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = oauthPage.Execute(w, struct{ Remote, Password string }{Remote: remote, Password: password})
}

func (o *OAuth) token(r *http.Request) (string, bool) {
	_, password, ok := r.BasicAuth()
	if !ok || password == "" {
		return "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.sessions[password]
	if !ok || time.Now().After(s.expires) {
		delete(o.sessions, password)
		return "", false
	}
	return s.token, true
}

var oauthPage = template.Must(template.New("oauth").Parse(`<!doctype html><title>subgit authorization complete</title><h1>GitHub authorization complete</h1><p>Configure this repository's push URL. The generated password expires in eight hours; repeat this authorization when it expires.</p><pre>git remote set-url --push origin https://oauth2:{{.Password}}@{{.Remote}}</pre>`))
