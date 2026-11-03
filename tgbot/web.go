package main

// Веб-пульт колонки: те же функции, что у Telegram-бота, но прямо в домашней сети —
// без Telegram и без VPN. Открывать с телефона: http://<IP колонки>:8088
// WEB_PIN=1234 в tgbot.conf включает вход по PIN (cookie на 180 дней).

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var webIndex []byte

var (
	webPIN  string
	webPort = "8088"

	sessMu   sync.Mutex
	sessions = map[string]time.Time{}
)

func newSession() string {
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	sessMu.Lock()
	sessions[id] = time.Now().Add(180 * 24 * time.Hour)
	sessMu.Unlock()
	return id
}

func authed(r *http.Request) bool {
	if webPIN == "" {
		return true
	}
	c, err := r.Cookie("st2")
	if err != nil {
		return false
	}
	sessMu.Lock()
	defer sessMu.Unlock()
	exp, ok := sessions[c.Value]
	return ok && time.Now().Before(exp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request) map[string]string {
	m := map[string]string{}
	var raw map[string]any
	if json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16*1024)).Decode(&raw) == nil {
		for k, v := range raw {
			switch t := v.(type) {
			case string:
				m[k] = t
			case float64:
				m[k] = strconv.FormatFloat(t, 'f', -1, 64)
			case bool:
				m[k] = strconv.FormatBool(t)
			}
		}
	}
	return m
}

func volumePercent() int {
	v := volume()
	if i := strings.Index(v, "["); i >= 0 {
		if j := strings.Index(v[i:], "%"); j > 0 {
			n, _ := strconv.Atoi(v[i+1 : i+j])
			return n
		}
	}
	return -1
}

func toggleFile(name string) string {
	return apDir + "/etc/tgbot." + name + "-off"
}

func setFlag(name string, on bool) {
	if on {
		os.Remove(toggleFile(name))
	} else {
		os.WriteFile(toggleFile(name), nil, 0600)
	}
}

func webState() map[string]any {
	nbMu.Lock()
	inst := nbInstruments[nbInst].id
	nbMu.Unlock()
	return map[string]any{
		"volume":     volumePercent(),
		"noteblock":  nbActive(),
		"instrument": inst,
		"earcon":     earconOn(),
		"notify":     notifyOn(),
		"telegram":   token != "",
	}
}

func serveWeb() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(webIndex)
	})

	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		pin := readJSON(r)["pin"]
		if webPIN == "" || subtle.ConstantTimeCompare([]byte(pin), []byte(webPIN)) == 1 {
			http.SetCookie(w, &http.Cookie{Name: "st2", Value: newSession(), Path: "/",
				MaxAge: 180 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		time.Sleep(time.Second) // перебор PIN — медленно
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"ok": false, "error": "Неверный PIN"})
	})

	api := func(path string, h func(w http.ResponseWriter, r *http.Request, in map[string]string)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if !authed(r) {
				w.WriteHeader(http.StatusUnauthorized)
				writeJSON(w, map[string]any{"ok": false, "auth": true})
				return
			}
			in := map[string]string{}
			if r.Method == http.MethodPost {
				in = readJSON(r)
			}
			defer func() {
				if e := recover(); e != nil {
					log.Printf("web %s panic: %v", path, e)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}()
			h(w, r, in)
		})
	}
	fail := func(w http.ResponseWriter, err error) {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
	}

	api("/api/state", func(w http.ResponseWriter, r *http.Request, in map[string]string) {
		s := webState()
		s["ok"] = true
		s["pin"] = webPIN != ""
		writeJSON(w, s)
	})

	api("/api/alice", func(w http.ResponseWriter, r *http.Request, in map[string]string) {
		t := strings.TrimSpace(in["text"])
		if t == "" || len([]rune(t)) > 500 {
			writeJSON(w, map[string]any{"ok": false, "error": "Нужен текст до 500 символов"})
			return
		}
		a, err := askAliceAnswer(t)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "answer": a})
	})

	api("/api/say", func(w http.ResponseWriter, r *http.Request, in map[string]string) {
		t := strings.TrimSpace(in["text"])
		if t == "" || len([]rune(t)) > 1000 {
			writeJSON(w, map[string]any{"ok": false, "error": "Нужен текст до 1000 символов"})
			return
		}
		res, err := say(t)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "answer": res})
	})

	api("/api/volume", func(w http.ResponseWriter, r *http.Request, in map[string]string) {
		if v, ok := in["set"]; ok {
			n, err := strconv.Atoi(v)
			if err == nil && n >= 0 && n <= 100 {
				amixer("-q", "set", "Master", strconv.Itoa(n)+"%")
			}
		} else if d, ok := in["delta"]; ok {
			n, err := strconv.Atoi(d)
			if err == nil && n != 0 && n >= -50 && n <= 50 {
				setVolumeDelta(n)
			}
		}
		writeJSON(w, map[string]any{"ok": true, "volume": volumePercent()})
	})

	api("/api/player", func(w http.ResponseWriter, r *http.Request, in map[string]string) {
		key := map[string]string{"toggle": "p", "pause": "o", "next": "n", "prev": "l", "stop": "c"}[in["action"]]
		if key == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "неизвестное действие"})
			return
		}
		if err := aliceCLI(key); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	api("/api/timer", func(w http.ResponseWriter, r *http.Request, in map[string]string) {
		n, err := strconv.Atoi(in["minutes"])
		if err != nil || n <= 0 || n > 600 {
			writeJSON(w, map[string]any{"ok": false, "error": "Минуты от 1 до 600"})
			return
		}
		a, err := askAliceAnswer("поставь таймер на " + strconv.Itoa(n) + " минут")
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "answer": a})
	})

	api("/api/alarm", func(w http.ResponseWriter, r *http.Request, in map[string]string) {
		hh, mm, ok := strings.Cut(strings.ReplaceAll(in["time"], ".", ":"), ":")
		h, e1 := strconv.Atoi(strings.TrimSpace(hh))
		m, e2 := strconv.Atoi(strings.TrimSpace(mm))
		if !ok || e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			writeJSON(w, map[string]any{"ok": false, "error": "Время в виде 7:30"})
			return
		}
		a, err := askAliceAnswer("поставь будильник на " + strconv.Itoa(h) + ":" + twoDigits(m))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "answer": a})
	})

	api("/api/status", func(w http.ResponseWriter, r *http.Request, in map[string]string) {
		writeJSON(w, map[string]any{"ok": true, "text": sh(30*time.Second, apDir+"/ap2.sh", "status")})
	})

	api("/api/restart", func(w http.ResponseWriter, r *http.Request, in map[string]string) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		sh(90*time.Second, apDir+"/ap2.sh", "restart")
		writeJSON(w, map[string]any{"ok": true})
	})

	api("/api/toggle", func(w http.ResponseWriter, r *http.Request, in map[string]string) {
		on := in["on"] == "true"
		switch in["name"] {
		case "noteblock":
			if on {
				inst := nbInstrumentIndex(in["instrument"])
				if err := nbStart(inst); err != nil {
					fail(w, err)
					return
				}
			} else {
				nbStop()
			}
		case "instrument":
			if i := nbInstrumentIndex(in["instrument"]); i >= 0 {
				nbMu.Lock()
				nbInst, nbNote = i, -1
				nbMu.Unlock()
				go nbPlay(i, 12)
			}
		case "earcon":
			setFlag("earcon", on)
		case "notify":
			setFlag("notify", on)
		default:
			writeJSON(w, map[string]any{"ok": false, "error": "неизвестный переключатель"})
			return
		}
		s := webState()
		s["ok"] = true
		writeJSON(w, s)
	})

	srv := &http.Server{
		Addr:              ":" + webPort,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("веб-пульт на порту %s", webPort)
	for {
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("веб-пульт: %v", err)
		}
		time.Sleep(10 * time.Second)
	}
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
