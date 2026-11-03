package main

// Свой звук отклика на «Алиса»: штатный сигнал активации на этой прошивке не играет
// (настройка в приложении включена, но в журнале maind звука нет). Слушаем состояние
// Алисы в локальном канале 9875 (как alice-text: 253=3, 252→1 — статус, 1 = слушает)
// и на переходе в «слушает» играем свой файл через gst прошивки.
// Выключить: файл etc/tgbot.earcon-off (команда /earcon off).

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"time"
)

const earconFile = "/system/vendor/quasar/sounds/common/spotter_start.mp3"

func earconOn() bool {
	_, err := os.Stat(apDir + "/etc/tgbot.earcon-off")
	return err != nil
}

func playEarcon() {
	cmd := exec.Command("/system/vendor/quasar/maind", "--gst-launch",
		"filesrc location="+earconFile+" ! decodebin ! audioconvert ! audioresample ! alsasink")
	cmd.Dir = "/system/vendor/quasar"
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=/system/vendor/quasar:libs")
	cmd.Run()
}

func watchAliceState() {
	for {
		conn, err := net.DialTimeout("tcp", ipcAddr, 3*time.Second)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		last := int64(-1)
		hdr := make([]byte, 4)
		for {
			if _, err := io.ReadFull(conn, hdr); err != nil {
				break
			}
			n := binary.LittleEndian.Uint32(hdr)
			if n == 0 || n > 4*1024*1024 {
				break
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(conn, buf); err != nil {
				break
			}
			ints, raw := fields(buf)
			if ints[253] != 3 {
				continue
			}
			st, _ := fields(raw[252])
			status := int64(st[1]) // нет поля — 0 (свободна)
			if status == 1 && last != 1 && earconOn() && !nbActive() {
				go playEarcon()
			}
			last = status
		}
		conn.Close()
		time.Sleep(2 * time.Second)
	}
}
