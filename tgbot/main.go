// tgbot — Telegram-бот на самой Станции 2: озвучка текста голосом Алисы, громкость,
// статус и перезапуск AirPlay/Spotify. Long polling, без входящих портов.
// Слушается только одного Telegram-пользователя (ALLOWED_ID в конфиге).
//
// Конфиг: /data/local/opt/ap2/etc/tgbot.conf
//
//	TOKEN=123456:ABC...
//	ALLOWED_ID=123456789
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	confPath = "/data/local/opt/ap2/etc/tgbot.conf"
	apDir    = "/data/local/opt/ap2"
	ipcAddr  = "127.0.0.1:9875"
)

var (
	token     string
	allowedID int64
	client    = &http.Client{Timeout: 70 * time.Second}
)

func loadConf() error {
	f, err := os.Open(confPath)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "TOKEN":
			token = v
		case "ALLOWED_ID":
			allowedID, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if token == "" || allowedID == 0 {
		return errors.New("в конфиге нужны TOKEN и ALLOWED_ID")
	}
	return nil
}

// ---- Telegram API ----

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From *struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
}

func api(method string, params url.Values, out any) error {
	resp, err := client.PostForm("https://api.telegram.org/bot"+token+"/"+method, params)
	if err != nil {
		// в тексте ошибки net/http есть URL с токеном — его не логируем
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s: %v", method, ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	if !r.OK {
		return fmt.Errorf("%s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func send(chat int64, text string) {
	if len(text) > 3900 {
		text = text[:3900] + "\n…"
	}
	if err := api("sendMessage", url.Values{"chat_id": {strconv.FormatInt(chat, 10)}, "text": {text}}, nil); err != nil {
		log.Printf("sendMessage: %v", err)
	}
}

// ---- локальный канал Алисы (127.0.0.1:9875) ----
// Кадр: 4 байта длины (LE) + protobuf. Формат START_VOCALIZE взят из alice-say.py
// (yandex-station-extended, проверено на Лайт 2).

func varint(v uint64) []byte {
	var b []byte
	for v >= 128 {
		b = append(b, byte(v&127|128))
		v >>= 7
	}
	return append(b, byte(v))
}
func pbInt(field, v uint64) []byte { return append(varint(field*8), varint(v)...) }
func pbBytes(field uint64, v []byte) []byte {
	b := append(varint(field*8+2), varint(uint64(len(v)))...)
	return append(b, v...)
}
func rpcFrame(field uint64, payload []byte) []byte {
	remote := append(pbBytes(7, []byte("AliceCapability")), pbBytes(field, payload)...)
	env := append(pbBytes(252, remote), pbInt(253, 210)...)
	out := make([]byte, 4, 4+len(env))
	binary.LittleEndian.PutUint32(out, uint32(len(env)))
	return append(out, env...)
}

// fields — верхний уровень protobuf: varint-поля в ints, length-delimited — в raw
func fields(b []byte) (ints map[uint64]uint64, raw map[uint64][]byte) {
	ints, raw = map[uint64]uint64{}, map[uint64][]byte{}
	read := func() (uint64, bool) {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			return 0, false
		}
		b = b[n:]
		return v, true
	}
	for len(b) > 0 {
		key, ok := read()
		if !ok {
			return
		}
		f := key >> 3
		switch key & 7 {
		case 0:
			v, ok := read()
			if !ok {
				return
			}
			ints[f] = v
		case 1:
			if len(b) < 8 {
				return
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return
			}
			b = b[4:]
		case 2:
			l, ok := read()
			if !ok || l > uint64(len(b)) {
				return
			}
			raw[f] = b[:l]
			b = b[l:]
		default:
			return
		}
	}
	return
}

func say(text string) (string, error) {
	conn, err := net.DialTimeout("tcp", ipcAddr, 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("локальный канал Алисы недоступен: %v", err)
	}
	defer conn.Close()
	directive := append(pbBytes(1, []byte(text)), pbInt(2, 1)...)
	payload := append(append(pbInt(1, 18), pbInt(3, 1)...), pbBytes(13, directive)...)
	req := append(rpcFrame(20, pbBytes(1, []byte("AliceCapability"))), rpcFrame(8, payload)...)
	if _, err := conn.Write(req); err != nil {
		return "", err
	}
	deadline := time.Now().Add(25 * time.Second)
	conn.SetReadDeadline(deadline)
	started := false
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			if started {
				return "Озвучка началась (конец не подтверждён).", nil
			}
			return "Запрос отправлен, подтверждения от Алисы не было.", nil
		}
		n := binary.LittleEndian.Uint32(hdr)
		if n == 0 || n > 512*1024 {
			return "", errors.New("странный ответ колонки")
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "Запрос отправлен, подтверждения от Алисы не было.", nil
		}
		ints, raw := fields(buf)
		remInts, remRaw := fields(raw[252])
		_ = remInts
		tag := ints[253]
		if tag != 3 && !(tag == 210 && string(remRaw[7]) == "AliceCapability") {
			continue
		}
		ev, ok := remRaw[18]
		if !ok {
			continue
		}
		evInts, _ := fields(ev)
		switch evInts[1] {
		case 6:
			started = true
		case 1:
			if started {
				return "Сказала.", nil
			}
		case 4:
			return "", errors.New("Алиса сообщила об ошибке озвучки")
		}
	}
}

// ---- команды колонки ----

func sh(timeout time.Duration, name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "PATH=/sbin:/usr/sbin:/bin:/usr/bin", "ALSA_CONFIG_PATH="+apDir+"/etc/asound.conf")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err.Error()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout):
		cmd.Process.Kill()
		out.WriteString("\n(прервано по таймауту)")
	}
	return strings.TrimSpace(out.String())
}

func amixer(args ...string) string {
	return sh(10*time.Second, apDir+"/bin/amixer", append([]string{"-D", "default"}, args...)...)
}

func volume() string {
	out := amixer("get", "Master")
	for _, l := range strings.Split(out, "\n") {
		if i := strings.Index(l, "["); i >= 0 && strings.Contains(l, "%") {
			return strings.TrimSpace(l[i:])
		}
	}
	return out
}

const help = `Команды:
/say текст — Алиса произнесёт текст (или просто напиши текст)
/vol — текущая громкость, /vol 40 — поставить 40%
/status — статус AirPlay/Spotify
/restart — перезапустить AirPlay/Spotify
/ip — адрес колонки`

func handle(chat int64, text string) {
	text = strings.TrimSpace(text)
	cmd, arg, _ := strings.Cut(text, " ")
	cmd = strings.ToLower(cmd)
	if i := strings.Index(cmd, "@"); i > 0 {
		cmd = cmd[:i]
	}
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "/start", "/help":
		send(chat, help)
	case "/vol":
		if arg == "" {
			send(chat, "Громкость: "+volume())
			return
		}
		n, err := strconv.Atoi(strings.TrimSuffix(arg, "%"))
		if err != nil || n < 0 || n > 100 {
			send(chat, "Нужно число от 0 до 100")
			return
		}
		amixer("-q", "set", "Master", strconv.Itoa(n)+"%")
		send(chat, "Громкость: "+volume())
	case "/status":
		send(chat, sh(30*time.Second, apDir+"/ap2.sh", "status"))
	case "/restart":
		send(chat, "Перезапускаю…")
		sh(90*time.Second, apDir+"/ap2.sh", "restart")
		send(chat, "Готово.\n"+sh(30*time.Second, apDir+"/ap2.sh", "status"))
	case "/ip":
		send(chat, sh(10*time.Second, "sh", "-c", "ip -4 addr show wlan0 2>/dev/null | awk '/inet /{print $2}' || ifconfig wlan0"))
	case "/say":
		if arg == "" {
			send(chat, "Напиши: /say текст")
			return
		}
		doSay(chat, arg)
	default:
		if strings.HasPrefix(text, "/") {
			send(chat, "Не знаю такую команду.\n\n"+help)
			return
		}
		doSay(chat, text)
	}
}

func doSay(chat int64, text string) {
	if len([]rune(text)) > 1000 {
		send(chat, "Слишком длинно, до 1000 символов")
		return
	}
	res, err := say(text)
	if err != nil {
		send(chat, "Не вышло: "+err.Error())
		return
	}
	send(chat, res)
}

func main() {
	log.SetFlags(log.LstdFlags)
	if err := loadConf(); err != nil {
		log.Fatalf("конфиг %s: %v", confPath, err)
	}
	log.Printf("tgbot запущен, разрешён пользователь %d", allowedID)
	var offset int64
	for {
		var ups []update
		err := api("getUpdates", url.Values{
			"timeout":         {"50"},
			"offset":          {strconv.FormatInt(offset, 10)},
			"allowed_updates": {`["message"]`},
		}, &ups)
		if err != nil {
			log.Printf("%v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil || m.From == nil || m.Text == "" {
				continue
			}
			if m.From.ID != allowedID {
				log.Printf("чужой пользователь %d — игнор", m.From.ID)
				continue
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("panic: %v", r)
					}
				}()
				handle(m.Chat.ID, m.Text)
			}()
		}
	}
}
