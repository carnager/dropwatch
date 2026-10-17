package server

import (
	"log"
	"net/http"
	"strings"
	"time"
)

const sessionCookie = "dropwatch_session"

// csrfHeader must accompany cookie-authenticated requests that change state.
// Browsers only send custom headers cross-origin after a CORS preflight,
// which this server never grants, so another site can't forge them.
const csrfHeader = "X-Dropwatch"

const minPasswordLength = 8

// publicAPI is reachable without logging in.
var publicAPI = map[string]bool{
	"GET /api/auth":         true,
	"POST /api/auth/setup":  true,
	"POST /api/auth/login":  true,
	"POST /api/auth/logout": true,
}

// requireAuth guards the API: a session cookie (browser) or the API token as
// "Authorization: Bearer <token>" (scripts, players). The web page itself is
// public; it shows the setup or login form until the API lets it in.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || publicAPI[r.Method+" "+r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			valid, err := s.store.ValidAPIToken(token)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if !valid {
				writeErr(w, http.StatusUnauthorized, "invalid API token")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		ok, err := s.validSession(r)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, "log in first")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) == "" {
			writeErr(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validSession(r *http.Request) (bool, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false, nil
	}
	return s.store.ValidSession(c.Value, time.Now())
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) error {
	token, err := s.store.CreateSession(time.Now())
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: int(30 * 24 * time.Hour / time.Second),
	})
	return nil
}

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	set, err := s.store.HasAdminPassword()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok, err := s.validSession(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"setupRequired": !set, "authenticated": set && ok})
}

// handleAuthSetup sets the first admin password. Until then whoever reaches
// the page first gets to set it, as with any self-hosted first-run screen.
func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeOne(w, r, 4096, &req) {
		return
	}
	if len(req.Password) < minPasswordLength {
		writeErr(w, http.StatusBadRequest, "use at least 8 characters")
		return
	}
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	set, err := s.store.HasAdminPassword()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if set {
		writeErr(w, http.StatusConflict, "a password is already set; log in instead")
		return
	}
	if err := s.store.SetAdminPassword(req.Password); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.startSession(w, r); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Print("admin password set")
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeOne(w, r, 4096, &req) {
		return
	}
	s.loginMu.Lock()
	ok, err := s.store.CheckAdminPassword(req.Password)
	if err == nil && !ok {
		time.Sleep(time.Second) // under the lock: one guess per second at most
	}
	s.loginMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		log.Printf("failed login from %s", r.RemoteAddr)
		writeErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	if err := s.startSession(w, r); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.store.DeleteSession(c.Value); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}

// handleChangePassword needs the current password even inside a session, so
// a forgotten open browser tab can't lock the owner out. All sessions end;
// this one gets a fresh login.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decodeOne(w, r, 4096, &req) {
		return
	}
	if len(req.New) < minPasswordLength {
		writeErr(w, http.StatusBadRequest, "use at least 8 characters")
		return
	}
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	ok, err := s.store.CheckAdminPassword(req.Current)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		time.Sleep(time.Second)
		writeErr(w, http.StatusUnauthorized, "current password is wrong")
		return
	}
	if err := s.store.SetAdminPassword(req.New); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.startSession(w, r); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleNewAPIToken(w http.ResponseWriter, r *http.Request) {
	token, err := s.store.NewAPIToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (s *Server) handleRevokeAPIToken(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RevokeAPIToken(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
