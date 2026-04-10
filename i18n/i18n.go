package i18n

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
)

var (
	translations = map[string]map[string]string{}
	mu           sync.RWMutex
)

// Load reads translation JSON files from dir (e.g. "./static/i18n").
func Load(dir string) error {
	for _, lang := range []string{"en", "fr"} {
		path := dir + "/" + lang + ".json"
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var m map[string]string
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
		mu.Lock()
		translations[lang] = m
		mu.Unlock()
	}
	return nil
}

// T returns the translation for key in lang.
// Falls back to English, then returns the key itself if not found.
func T(lang, key string) string {
	mu.RLock()
	m, ok := translations[lang]
	mu.RUnlock()
	if ok {
		if v, ok2 := m[key]; ok2 {
			return v
		}
	}
	mu.RLock()
	en := translations["en"]
	mu.RUnlock()
	if v, ok := en[key]; ok {
		return v
	}
	return key
}

// TranslationsJSON returns the translations for lang as a JSON string (for JS injection).
func TranslationsJSON(lang string) string {
	mu.RLock()
	m, ok := translations[lang]
	mu.RUnlock()
	if !ok {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// DetectLang detects the preferred language from the request:
// 1. URL param ?lang=xx
// 2. Cookie "lang"
// 3. Accept-Language header
// 4. Default "en"
func DetectLang(r *http.Request) string {
	if l := r.URL.Query().Get("lang"); l == "en" || l == "fr" {
		return l
	}
	if c, err := r.Cookie("lang"); err == nil {
		if c.Value == "en" || c.Value == "fr" {
			return c.Value
		}
	}
	al := r.Header.Get("Accept-Language")
	if strings.Contains(strings.ToLower(al), "fr") {
		return "fr"
	}
	return "en"
}

// HandleSetLang handles POST /set-lang with body {"lang":"en"} or {"lang":"fr"}.
// Sets a cookie "lang" valid for 1 year and returns 200 OK.
func HandleSetLang(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req struct {
		Lang string `json:"lang"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if req.Lang != "en" && req.Lang != "fr" {
		http.Error(w, "invalid lang", 400)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "lang",
		Value:    req.Lang,
		Path:     "/",
		MaxAge:   365 * 24 * 60 * 60,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusOK)
}
