package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"scorbits/db"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type Session struct {
	UserID    bson.ObjectID
	ExpiresAt time.Time
}

var (
	sessions = make(map[string]*Session)
	mu       sync.RWMutex
)

const sessionDuration = 7 * 24 * time.Hour
const cookieName = "sco_session"

func HashPassword(password string) string {
	h := sha256.Sum256([]byte(password + "scorbits_salt_2026"))
	return hex.EncodeToString(h[:])
}

func GenerateToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func CreateSession(w http.ResponseWriter, userID bson.ObjectID) string {
	token := GenerateToken()
	mu.Lock()
	sessions[token] = &Session{
		UserID:    userID,
		ExpiresAt: time.Now().Add(sessionDuration),
	}
	mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionDuration.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return token
}

func GetSession(r *http.Request) (*db.User, error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return nil, fmt.Errorf("pas de session")
	}

	mu.RLock()
	session, exists := sessions[cookie.Value]
	mu.RUnlock()

	if !exists || time.Now().After(session.ExpiresAt) {
		return nil, fmt.Errorf("session expirée")
	}

	return db.GetUserByID(session.UserID)
}

func DestroySession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(cookieName)
	if err == nil {
		mu.Lock()
		delete(sessions, cookie.Value)
		mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:   cookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
}

func RequireAuth(w http.ResponseWriter, r *http.Request) (*db.User, bool) {
	user, err := GetSession(r)
	if err != nil {
		http.Redirect(w, r, "/wallet", 302)
		return nil, false
	}
	return user, true
}