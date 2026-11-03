// tgbot — Telegram-бот на самой Станции 2: команды Алисе текстом, плеер, таймеры,
// озвучка текста, громкость, статус AirPlay/Spotify, уведомления. Long polling, без
// входящих портов. Команды Алисе и плеер — через встроенный пульт прошивки
// (maind --cli), озвучка — через локальный канал 127.0.0.1:9875.
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
	"sync"
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

type chatRef struct {
	ID int64 `json:"id"`
}

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64   `json:"message_id"`
		Chat      chatRef `json:"chat"`
		From      *struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
	Callback *struct {
		ID   string `json:"id"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Message *struct {
			MessageID int64   `json:"message_id"`
			Chat      chatRef `json:"chat"`
		} `json:"message"`
		Data string `json:"data"`
	} `json:"callback_query"`
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

// ---- встроенный пульт прошивки: maind --cli ----
// Команды пульта — по символу на строку: q — текстовый запрос Алисе (следующей строкой
// текст), p — пауза/продолжить, o — пауза, n — дальше, l — назад, c — прервать Алису.
// Проверено на Станции 2: запрос уходит в Алису как Vins.TextInput, без облачных токенов.

var cliMu sync.Mutex

func aliceCLI(lines ...string) error {
	cliMu.Lock()
	defer cliMu.Unlock()
	cmd := exec.Command("/system/vendor/quasar/maind", "--cli")
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=/system/vendor/quasar/libs")
	cmd.Dir = "/tmp"
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("пульт Алисы не запустился: %v", err)
	}
	// пульту нужно подключиться к демонам, иначе первые символы теряются
	time.Sleep(2 * time.Second)
	for _, l := range lines {
		io.WriteString(in, l+"\n")
		time.Sleep(700 * time.Millisecond)
	}
	// даём команде уйти и закрываем пульт
	time.Sleep(3 * time.Second)
	io.WriteString(in, "x\n")
	in.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		<-done
	}
	return nil
}

func askAlice(text string) error {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return errors.New("пустой запрос")
	}
	return aliceCLI("q", text)
}

// ---- клавиатура ----

func notifyOn() bool {
	_, err := os.Stat(apDir + "/etc/tgbot.notify-off")
	return err != nil
}

func menuMarkup() string {
	n := "🔔 Уведомления: вкл"
	if !notifyOn() {
		n = "🔕 Уведомления: выкл"
	}
	kb := [][]map[string]string{
		{{"text": "🔉 −10", "callback_data": "vol-"}, {"text": "🔊 +10", "callback_data": "vol+"}, {"text": "🔈 Громкость?", "callback_data": "vol?"}},
		{{"text": "⏮", "callback_data": "prev"}, {"text": "⏯", "callback_data": "toggle"}, {"text": "⏭", "callback_data": "next"}},
		{{"text": "⏱ Таймер 5 мин", "callback_data": "timer5"}, {"text": "⏱ 10 мин", "callback_data": "timer10"}, {"text": "⏰ Будильники", "callback_data": "alarms"}},
		{{"text": "🤫 Замолчи", "callback_data": "cancel"}, {"text": "📊 Статус", "callback_data": "status"}},
		{{"text": "🔄 Перезапустить AirPlay/Spotify", "callback_data": "restart"}},
		{{"text": n, "callback_data": "notify"}},
	}
	b, _ := json.Marshal(map[string]any{"inline_keyboard": kb})
	return string(b)
}

func sendMenu(chat int64) {
	params := url.Values{
		"chat_id":      {strconv.FormatInt(chat, 10)},
		"text":         {"Пульт колонки. Любой текст без / — команда Алисе, как голосом."},
		"reply_markup": {menuMarkup()},
	}
	if err := api("sendMessage", params, nil); err != nil {
		log.Printf("sendMessage: %v", err)
	}
}

func toast(id, text string) {
	api("answerCallbackQuery", url.Values{"callback_query_id": {id}, "text": {text}}, nil)
}

func setVolumeDelta(d int) string {
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	amixer("-q", "set", "Master", strconv.Itoa(d)+"%"+sign)
	return "Громкость: " + volume()
}

func handleCallback(id string, chat, msgID int64, data string) {
	switch data {
	case "vol+":
		toast(id, setVolumeDelta(10))
	case "vol-":
		toast(id, setVolumeDelta(-10))
	case "vol?":
		toast(id, "Громкость: "+volume())
	case "toggle", "next", "prev":
		toast(id, "Отправляю…")
		key := map[string]string{"toggle": "p", "next": "n", "prev": "l"}[data]
		if err := aliceCLI(key); err != nil {
			send(chat, "Не вышло: "+err.Error())
		}
	case "cancel":
		toast(id, "Тихо…")
		aliceCLI("c")
	case "timer5", "timer10":
		m := strings.TrimPrefix(data, "timer")
		toast(id, "Ставлю таймер на "+m+" мин")
		if err := askAlice("поставь таймер на " + m + " минут"); err != nil {
			send(chat, "Не вышло: "+err.Error())
		}
	case "alarms":
		toast(id, "Алиса скажет вслух")
		askAlice("какие у меня будильники")
	case "status":
		toast(id, "")
		send(chat, sh(30*time.Second, apDir+"/ap2.sh", "status"))
	case "restart":
		toast(id, "Перезапускаю, ~30 с")
		sh(90*time.Second, apDir+"/ap2.sh", "restart")
		send(chat, "AirPlay/Spotify перезапущены.")
	case "notify":
		f := apDir + "/etc/tgbot.notify-off"
		if notifyOn() {
			os.WriteFile(f, nil, 0600)
			toast(id, "Уведомления выключены")
		} else {
			os.Remove(f)
			toast(id, "Уведомления включены")
		}
		api("editMessageReplyMarkup", url.Values{
			"chat_id":      {strconv.FormatInt(chat, 10)},
			"message_id":   {strconv.FormatInt(msgID, 10)},
			"reply_markup": {menuMarkup()},
		}, nil)
	default:
		toast(id, "")
	}
}

// ---- текстовые команды ----

const help = `Пульт — /menu (кнопки).

Любой текст без / — команда Алисе, как будто сказал голосом:
  «включи свет», «какая погода», «поставь будильник на 7:30»

/timer 15 — таймер на 15 минут
/alarm 7:30 — будильник на 7:30
/alarms — Алиса назовёт будильники
/pause /play /next /prev — плеер Алисы
/stop — прервать Алису
/say текст — произнести текст голосом Алисы (без выполнения)
/vol, /vol 40 — громкость
/status, /restart — AirPlay/Spotify
/notify on|off — уведомления
/ip — адрес колонки`

func handle(chat int64, text string) {
	text = strings.TrimSpace(text)
	cmd, arg, _ := strings.Cut(text, " ")
	cmd = strings.ToLower(cmd)
	if i := strings.Index(cmd, "@"); i > 0 {
		cmd = cmd[:i]
	}
	arg = strings.TrimSpace(arg)
	alice := func(q, ok string) {
		if err := askAlice(q); err != nil {
			send(chat, "Не вышло: "+err.Error())
			return
		}
		send(chat, ok)
	}
	player := func(key, ok string) {
		if err := aliceCLI(key); err != nil {
			send(chat, "Не вышло: "+err.Error())
			return
		}
		send(chat, ok)
	}
	switch cmd {
	case "/start", "/menu":
		sendMenu(chat)
	case "/help":
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
	case "/alice", "/a":
		if arg == "" {
			send(chat, "Напиши: /alice команда")
			return
		}
		alice(arg, "Передал Алисе.")
	case "/timer":
		n, err := strconv.Atoi(strings.TrimSuffix(strings.Fields(arg + " x")[0], "м"))
		if err != nil || n <= 0 || n > 600 {
			send(chat, "Напиши: /timer 15 (минуты)")
			return
		}
		alice(fmt.Sprintf("поставь таймер на %d минут", n), fmt.Sprintf("Таймер на %d мин поставлен (Алиса подтвердит голосом).", n))
	case "/alarm":
		hh, mm, ok := strings.Cut(strings.ReplaceAll(arg, ".", ":"), ":")
		h, e1 := strconv.Atoi(strings.TrimSpace(hh))
		m, e2 := strconv.Atoi(strings.TrimSpace(mm))
		if !ok || e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			send(chat, "Напиши: /alarm 7:30")
			return
		}
		alice(fmt.Sprintf("поставь будильник на %d:%02d", h, m), fmt.Sprintf("Будильник на %d:%02d поставлен (Алиса подтвердит голосом).", h, m))
	case "/alarms":
		alice("какие у меня будильники", "Алиса назовёт будильники вслух.")
	case "/timers":
		alice("сколько осталось на таймере", "Алиса скажет вслух.")
	case "/pause":
		player("o", "Пауза.")
	case "/play":
		player("p", "Пауза/продолжить.")
	case "/next":
		player("n", "Следующий трек.")
	case "/prev":
		player("l", "Предыдущий трек.")
	case "/stop":
		player("c", "Алиса прервана.")
	case "/notify":
		f := apDir + "/etc/tgbot.notify-off"
		switch strings.ToLower(arg) {
		case "off", "выкл":
			os.WriteFile(f, nil, 0600)
		case "on", "вкл":
			os.Remove(f)
		}
		if notifyOn() {
			send(chat, "Уведомления включены. /notify off — выключить.")
		} else {
			send(chat, "Уведомления выключены. /notify on — включить.")
		}
	default:
		if strings.HasPrefix(text, "/") {
			send(chat, "Не знаю такую команду.\n\n"+help)
			return
		}
		if len([]rune(text)) > 500 {
			send(chat, "Слишком длинно для Алисы, до 500 символов")
			return
		}
		alice(text, "Передал Алисе.")
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

// ---- уведомления: следим за журналом распорядителя звука ----

const ownerLog = "/data/local/tmp/ap2/owner.log"

var events = []struct{ match, text string }{
	{"AirPlay пошёл", "▶️ Подключились по AirPlay"},
	{"AirPlay закончился", "⏹ AirPlay отключился"},
	{"играет Spotify", "▶️ Играет Spotify"},
	{"играет YouTube Music", "▶️ Играет YouTube Music"},
	{"приёмник AirPlay пропал", "⚠️ Приёмник AirPlay упал — поднимаю заново"},
	{"librespot пропал", "⚠️ Spotify Connect упал — поднимаю заново"},
}

func watchOwnerLog() {
	var off int64 = -1
	var last string
	var lastAt time.Time
	for {
		time.Sleep(3 * time.Second)
		st, err := os.Stat(ownerLog)
		if err != nil {
			continue
		}
		if off < 0 || st.Size() < off {
			// первый запуск — начинаем с конца; файл обрезали — с начала
			if off < 0 {
				off = st.Size()
				continue
			}
			off = 0
		}
		if st.Size() == off {
			continue
		}
		f, err := os.Open(ownerLog)
		if err != nil {
			continue
		}
		f.Seek(off, io.SeekStart)
		data, _ := io.ReadAll(io.LimitReader(f, 64*1024))
		f.Close()
		// читаем только целые строки
		if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
			data = data[:i+1]
		} else {
			continue
		}
		off += int64(len(data))
		if !notifyOn() {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			for _, e := range events {
				if !strings.Contains(line, e.match) {
					continue
				}
				// одинаковые события подряд в течение минуты не дублируем
				if e.text == last && time.Since(lastAt) < time.Minute {
					break
				}
				last, lastAt = e.text, time.Now()
				send(allowedID, e.text)
				break
			}
		}
	}
}

func bootNotify() {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return
	}
	sec, _ := strconv.ParseFloat(strings.Fields(string(b) + " 0")[0], 64)
	if sec < 300 && notifyOn() {
		send(allowedID, "🔌 Колонка включилась и на связи. /menu — пульт")
	}
}

func setCommands() {
	cmds := []map[string]string{
		{"command": "menu", "description": "Пульт с кнопками"},
		{"command": "timer", "description": "Таймер: /timer 15"},
		{"command": "alarm", "description": "Будильник: /alarm 7:30"},
		{"command": "pause", "description": "Пауза"},
		{"command": "next", "description": "Следующий трек"},
		{"command": "vol", "description": "Громкость: /vol 40"},
		{"command": "say", "description": "Произнести текст"},
		{"command": "status", "description": "Статус AirPlay/Spotify"},
		{"command": "help", "description": "Все команды"},
	}
	b, _ := json.Marshal(cmds)
	if err := api("setMyCommands", url.Values{"commands": {string(b)}}, nil); err != nil {
		log.Printf("setMyCommands: %v", err)
	}
}

func safe(f func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v", r)
		}
	}()
	f()
}

func main() {
	log.SetFlags(log.LstdFlags)
	if err := loadConf(); err != nil {
		log.Fatalf("конфиг %s: %v", confPath, err)
	}
	log.Printf("tgbot запущен, разрешён пользователь %d", allowedID)
	go safe(watchOwnerLog)
	commandsSet, booted := false, false
	var offset int64
	for {
		var ups []update
		err := api("getUpdates", url.Values{
			"timeout":         {"50"},
			"offset":          {strconv.FormatInt(offset, 10)},
			"allowed_updates": {`["message","callback_query"]`},
		}, &ups)
		if err != nil {
			log.Printf("%v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if !commandsSet {
			commandsSet = true
			go safe(setCommands)
		}
		if !booted {
			booted = true
			go safe(bootNotify)
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			if c := u.Callback; c != nil {
				if c.From.ID != allowedID || c.Message == nil {
					toast(c.ID, "")
					continue
				}
				// кнопки отвечают сразу, медленные действия идут в фоне
				go safe(func() { handleCallback(c.ID, c.Message.Chat.ID, c.Message.MessageID, c.Data) })
				continue
			}
			m := u.Message
			if m == nil || m.From == nil || m.Text == "" {
				continue
			}
			if m.From.ID != allowedID {
				log.Printf("чужой пользователь %d — игнор", m.From.ID)
				continue
			}
			go safe(func() { handle(m.Chat.ID, m.Text) })
		}
	}
}
