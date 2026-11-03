package main

// Режим «нотный блок»: кнопка Алисы на колонке играет ноты из Minecraft.
// Короткое нажатие — следующая нота (24 полутона по кругу, как ПКМ по нотному блоку),
// долгое (>0,7 с) — следующий инструмент. Кнопки громкости работают как обычно.
// На время режима сенсорная панель захватывается (EVIOCGRAB), прошивка нажатий не
// видит; выключение — /noteblock off или сам через 15 минут без нажатий.

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	nbDevice   = "/dev/input/event1" // midi_touch: 582 — кнопка Алисы, 114/115 — громкость
	nbDir      = apDir + "/noteblock/notes"
	keyAlice   = 582
	keyVolDown = 114
	keyVolUp   = 115
	eviocgrab  = 0x40044590
	nbIdle     = 15 * time.Minute
)

var nbInstruments = []struct{ id, name string }{
	{"harp", "арфа"}, {"bell", "колокольчик"}, {"bass", "бас"}, {"flute", "флейта"},
	{"guitar", "гитара"}, {"xylo", "ксилофон"}, {"bit", "8-бит"},
}

var (
	nbMu      sync.Mutex
	nbFile    *os.File
	nbInst    int
	nbNote    = -1
	nbPlaying int32
)

func nbActive() bool {
	nbMu.Lock()
	defer nbMu.Unlock()
	return nbFile != nil
}

func nbInstrumentIndex(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	for i, in := range nbInstruments {
		if s == in.id || s == in.name {
			return i
		}
	}
	return -1
}

func nbPlay(inst, note int) {
	// не больше 4 нот одновременно, иначе maind-ы забьют процессор
	if atomic.LoadInt32(&nbPlaying) >= 4 {
		return
	}
	atomic.AddInt32(&nbPlaying, 1)
	go func() {
		defer atomic.AddInt32(&nbPlaying, -1)
		f := fmt.Sprintf("%s/%s/%d.opus", nbDir, nbInstruments[inst].id, note)
		cmd := exec.Command("/system/vendor/quasar/maind", "--gst-launch",
			"filesrc location="+f+" ! decodebin ! audioconvert ! audioresample ! alsasink")
		cmd.Dir = "/system/vendor/quasar"
		cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=/system/vendor/quasar:libs")
		cmd.Run()
	}()
}

func nbStart(inst int) error {
	nbMu.Lock()
	defer nbMu.Unlock()
	if inst >= 0 {
		nbInst = inst
	}
	if nbFile != nil {
		return nil
	}
	f, err := os.Open(nbDevice)
	if err != nil {
		return err
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), eviocgrab, 1); e != 0 {
		f.Close()
		return fmt.Errorf("не удалось захватить кнопки: %v", e)
	}
	nbFile = f
	nbNote = -1
	go nbLoop(f)
	return nil
}

func nbStop() {
	nbMu.Lock()
	f := nbFile
	nbFile = nil
	nbMu.Unlock()
	if f != nil {
		syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), eviocgrab, 0)
		f.Close() // nbLoop выйдет на ошибке чтения
	}
}

func nbLoop(f *os.File) {
	buf := make([]byte, 24)
	var down time.Time
	last := time.Now()
	idle := time.AfterFunc(nbIdle, func() {
		if nbActive() {
			nbStop()
			send(allowedID, "🎵 Нотный блок выключен сам (15 минут без нажатий).")
		}
	})
	defer idle.Stop()
	for {
		if _, err := f.Read(buf); err != nil {
			return
		}
		typ := binary.LittleEndian.Uint16(buf[16:])
		code := binary.LittleEndian.Uint16(buf[18:])
		val := int32(binary.LittleEndian.Uint32(buf[20:]))
		if typ != 1 {
			continue
		}
		idle.Reset(nbIdle)
		last = time.Now()
		switch code {
		case keyAlice:
			if val == 1 {
				down = last
				continue
			}
			if val != 0 {
				continue
			}
			nbMu.Lock()
			if time.Since(down) > 700*time.Millisecond {
				nbInst = (nbInst + 1) % len(nbInstruments)
				nbNote = 12
			} else {
				nbNote = (nbNote + 1) % 25
			}
			inst, note := nbInst, nbNote
			nbMu.Unlock()
			nbPlay(inst, note)
		case keyVolUp, keyVolDown:
			if val == 0 {
				continue
			}
			if code == keyVolUp {
				amixer("-q", "set", "Master", "6%+")
			} else {
				amixer("-q", "set", "Master", "6%-")
			}
		}
	}
}

func handleNoteblock(chat int64, arg string) {
	a := strings.ToLower(strings.TrimSpace(arg))
	switch {
	case a == "off" || a == "выкл":
		nbStop()
		send(chat, "🎵 Нотный блок выключен, кнопка снова работает как обычно.")
	case a == "" && nbActive():
		nbStop()
		send(chat, "🎵 Нотный блок выключен.")
	default:
		inst := -1
		if a != "" && a != "on" && a != "вкл" {
			if inst = nbInstrumentIndex(a); inst < 0 {
				names := make([]string, len(nbInstruments))
				for i, in := range nbInstruments {
					names[i] = in.name
				}
				send(chat, "Инструменты: "+strings.Join(names, ", "))
				return
			}
		}
		if err := nbStart(inst); err != nil {
			log.Printf("noteblock: %v", err)
			send(chat, "Не вышло: "+err.Error())
			return
		}
		nbMu.Lock()
		name := nbInstruments[nbInst].name
		nbMu.Unlock()
		send(chat, "🎵 Нотный блок включён ("+name+").\n"+
			"Кнопка Алисы — следующая нота, долгое нажатие — другой инструмент, "+
			"громкость работает как обычно. Выключить: /noteblock off")
	}
}
