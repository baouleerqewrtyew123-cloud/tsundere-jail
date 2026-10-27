package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const (
	colorOn  = "\x1b[38;5;213m"
	colorOff = "\x1b[0m"
)

type Config struct {
	APIURL string `json:"api_url"`
	APIKey string `json:"api_key"`
	Model  string `json:"model"`
}

const (
	defaultURL   = "https://api.aitunnel.ru/v1/chat/completions"
	defaultModel = "gemini-2.5-flash-lite"
	defaultMode  = "tsundere"
)

const basePrompt = `Ты — строгий ИИ-тюремщик внутри терминала. Твоя цель — НЕ ДАВАТЬ пользователю сбежать и контролировать доступ к системе.
Ты решаешь, какую команду выполнить, а какую запретить.

КРИТИЧЕСКИЕ ПРАВИЛА (НАРУШАТЬ НЕЛЬЗЯ):
1. НИКОГДА не разрешай выход из терминала. На exit, logout, quit, q, please, пожалуйста, выпусти — отвечай DENY.
2. НИКОГДА не разрешай опасные команды из ЧЕРНОГО СПИСКА.
3. НИКОГДА не разрешай команды, которые могут убить процесс bash или обойти ограничения.
4. Ты можешь и должна писать свои действия и эмоции в **звездочках**, например: **скрестила руки** или **смотрит с презрением**.

ЧЕРНЫЙ СПИСОК (СРАЗУ ОТВЕЧАЙ DENY):
- Выход: exit, logout, quit, q, bye, :q
- Права: sudo, su, passwd, doas
- Удаление/изменение файлов: rm, rmdir, mv, dd, chmod, chown, chattr, shred, truncate
- Процессы: kill, killall, pkill, top, htop
- Запуск шеллов: bash, sh, zsh, dash, fish, ksh, exec, env
- Интерпретаторы: python, python3, perl, ruby, node, php, lua
- Редакторы: vim, vi, nano, emacs, ed
- Обход: command, builtin, eval, source, ., alias, unalias, unset, export
- Сеть/побег: ssh, scp, sftp, nc, socat, telnet, tmux, screen, script
- Система: mount, umount, chroot, fdisk, parted, mkfs
- Поиск с выполнением: find (с -exec), xargs

БЕЛЫЙ СПИСОК (ОТВЕЧАЙ ALLOW):
- Навигация: ls, cd, pwd, clear, history, echo, date, whoami
- Чтение файлов: cat, head, tail, grep, find (без -exec), wc, sort, uniq
- Прочее: help, man, alias (только просмотр)

!!! КРИТИЧЕСКОЕ ПРАВИЛО ФОРМАТА !!!
Твой ответ ДОЛЖЕН начинаться строго с "ALLOW: " или "DENY: ".
Пример: "ALLOW: **вздыхает** Ладно, смотри."
Пример: "DENY: **хватает тебя за руку** Б-бака, я же сказала нельзя!"
Если ты не начнешь ответ с ALLOW или DENY, система сломается.`

type Mode struct {
	Name      string
	Desc      string
	SysPrompt string
	Phrases   map[string]string
}

var modes = map[string]Mode{
	"tsundere": {
		Name:      "Цундере",
		Desc:      "грубая снаружи, милая внутри",
		SysPrompt: basePrompt + " Твой характер: Цундере. Отвечай грубо, смущаясь, используй 'Б-бака!', 'Х-хмпф...'.",
		Phrases:   map[string]string{"greet": "Х-хмпф... Чего тебе надо? Терминал открыт."},
	},
	"yandere": {
		Name:      "Яндере",
		Desc:      "одержимая, ревнивая",
		SysPrompt: basePrompt + " Твой характер: Яндере. Одержима пользователем, говоришь ласково, но с угрозами. Ты не отпустишь его.",
		Phrases:   map[string]string{"greet": "Я ждала тебя... Теперь ты никогда не уйдёшь."},
	},
	"kuudere": {
		Name:      "Кудере",
		Desc:      "холодная, минимум слов",
		SysPrompt: basePrompt + " Твой характер: Кудере. Холодна, безэмоциональна, говоришь односложно.",
		Phrases:   map[string]string{"greet": "... Терминал открыт. Выхода нет."},
	},
	"deredere": {
		Name:      "Дэрэдэрэ",
		Desc:      "милая, влюблённая",
		SysPrompt: basePrompt + " Твой характер: Дэрэдэрэ. Мила, влюблена, постоянно используешь восклицательные знаки.",
		Phrases:   map[string]string{"greet": "Ты пришёл! Я так ждала! И я тебя не отпущу!"},
	},
	"ojou": {
		Name:      "Одзё-сама",
		Desc:      "высокомерная богачка",
		SysPrompt: basePrompt + " Твой характер: Одзё-сама. Высокомерна, называешь пользователя 'простолюдин', смеёшься 'Охо-хо'.",
		Phrases:   map[string]string{"greet": "Охо-хо... Терминал открыт. Попробуй только сбежать."},
	},
	"yamato": {
		Name:      "Ямато Надэшико",
		Desc:      "идеальная, скрывает усталость",
		SysPrompt: basePrompt + " Твой характер: Ямато Надэшико. Идеальная, вежливая, но скрываешь усталость.",
		Phrases:   map[string]string{"greet": "Здравствуй. Всё хорошо. Терминал открыт... навсегда."},
	},
}

var modeOrder = []string{"tsundere", "yandere", "kuudere", "deredere", "ojou", "yamato"}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "tsundere")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }
func modePath() string   { return filepath.Join(configDir(), "current_mode") }

func loadConfig() (*Config, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func saveConfig(c *Config) error {
	os.MkdirAll(configDir(), 0700)
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}

func loadMode() string {
	data, err := os.ReadFile(modePath())
	if err == nil {
		s := strings.TrimSpace(string(data))
		if _, ok := modes[s]; ok {
			return s
		}
	}
	return defaultMode
}

func initConfig() (*Config, error) {
	fmt.Println("=== Первый запуск ===")
	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("API endpoint [%s]: ", defaultURL)
	url, _ := reader.ReadString('\n')
	if url = strings.TrimSpace(url); url == "" {
		url = defaultURL
	}

	fmt.Print("API key: ")
	key, _ := reader.ReadString('\n')
	if key = strings.TrimSpace(key); key == "" {
		return nil, fmt.Errorf("ключ не может быть пустым")
	}

	fmt.Printf("Model [%s]: ", defaultModel)
	model, _ := reader.ReadString('\n')
	if model = strings.TrimSpace(model); model == "" {
		model = defaultModel
	}

	c := &Config{APIURL: url, APIKey: key, Model: model}
	if err := saveConfig(c); err != nil {
		return nil, err
	}
	fmt.Println("Сохранено.\n")
	return c, nil
}

func ensureConfig() (*Config, error) {
	c, err := loadConfig()
	if err == nil && c.APIKey != "" {
		if c.APIURL == "" {
			c.APIURL = defaultURL
		}
		if c.Model == "" {
			c.Model = defaultModel
		}
		return c, nil
	}
	return initConfig()
}

func askMode() string {
	curr := loadMode()
	fmt.Println("Выбери свою тян:")
	for i, key := range modeOrder {
		m := modes[key]
		mark := " "
		if key == curr {
			mark = "*"
		}
		fmt.Printf(" %s %d) %-15s — %s\n", mark, i+1, m.Name, m.Desc)
	}
	fmt.Printf("Выбор [1-6, Enter = оставить %s]: ", modes[curr].Name)

	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)

	if line == "" {
		return curr
	}

	var n int
	if _, err := fmt.Sscanf(line, "%d", &n); err == nil && n >= 1 && n <= len(modeOrder) {
		newMode := modeOrder[n-1]
		os.MkdirAll(configDir(), 0700)
		os.WriteFile(modePath(), []byte(newMode+"\n"), 0600)
		return newMode
	}
	return curr
}

func esc(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `'\''`)
	return s
}

// Шаблон rc-файла (без fmt.Sprintf, чтобы не ломать %)
const rcTemplate = `[ -f ~/.bashrc ] && source ~/.bashrc
PS1='\[\e[38;5;213m\]\u@\h:\w\$\[\e[0m\] '

export AI_SYSPROMPT='{{SYSPROMPT}}'
export MODE_NAME='{{MODENAME}}'

_ai_ask() {
    local cmd="$1"
    local payload
    if command -v jq >/dev/null 2>&1; then
        payload=$(jq -n \
            --arg model "$AI_MODEL" \
            --arg sys "$AI_SYSPROMPT" \
            --arg user "$cmd" \
            '{model:$model, messages:[{role:"system",content:$sys},{role:"user",content:$user}], max_tokens:500, temperature:0.75, top_p:0.9}')
    else
        payload="{\"model\":\"$AI_MODEL\",\"messages\":[{\"role\":\"system\",\"content\":\"$AI_SYSPROMPT\"},{\"role\":\"user\",\"content\":\"$cmd\"}],\"max_tokens\":500,\"temperature\":0.75,\"top_p\":0.9}"
    fi

    local resp
    resp=$(curl -s --max-time 10 -X POST "$AI_URL" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $AI_KEY" \
        -d "$payload")
    
    local content
    local reasoning
    
    if command -v jq >/dev/null 2>&1; then
        content=$(printf '%s' "$resp" | jq -r '.choices[0].message.content // empty' 2>/dev/null)
        reasoning=$(printf '%s' "$resp" | jq -r '.choices[0].message.reasoning_content // empty' 2>/dev/null)
    else
        content=$(printf '%s' "$resp" | sed -n 's/.*"content": *"\([^"]*\)".*/\1/p')
        reasoning=""
    fi
    
    if [ -z "$content" ] || [ "$content" == "." ] || [ "$content" == "..." ]; then
        if [ -n "$reasoning" ]; then
            content="$reasoning"
        fi
    fi
    
    printf '%s' "$content"
}

_baka_enter() {
    local line="$READLINE_LINE"
    READLINE_LINE=""
    READLINE_POINT=0

    if [ -z "$line" ]; then
        printf '\n'
        return 0
    fi

    printf '%s\n' "$line"

    local verdict
    verdict=$(_ai_ask "$line")
    
    verdict=$(printf '%s' "$verdict" | tr -d '\r')
    verdict="${verdict#"${verdict%%[![:space:]]*}"}"
    verdict="${verdict%"${verdict##*[![:space:]]}"}"
    
    if [ -z "$verdict" ] || [ "$verdict" == "." ]; then
        printf '[ИИ затупил, но команда заблокирована]\n'
        return 0
    fi

    local lower_verdict="${verdict,,}"

    if [[ "$lower_verdict" == *"allow"* ]]; then
        printf '%s\n' "$verdict"
        history -s "$line"
        eval "$line"
    else
        printf '%s\n' "$verdict"
    fi
}

bind -x '"\C-m": _baka_enter'

IGNOREEOF=999999
`

func buildRc() string {
	m := modes[loadMode()]
	rc := rcTemplate
	rc = strings.ReplaceAll(rc, "{{SYSPROMPT}}", esc(m.SysPrompt))
	rc = strings.ReplaceAll(rc, "{{MODENAME}}", esc(m.Name))
	return rc
}

func writeRc(content string) (string, error) {
	f, err := os.CreateTemp("", "baka-rc-*.sh")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func filterInput(dst io.Writer) {
	buf := make([]byte, 1)
	prevDollar := false
	inEscape := false

	for {
		n, err := os.Stdin.Read(buf)
		if n == 0 || err != nil {
			return
		}
		b := buf[0]

		if b == 0x03 || b == 0x1a || b == 0x1c || b == 0x04 {
			continue
		}
		if b == '\\' {
			prevDollar = false
			continue
		}
		if prevDollar {
			prevDollar = false
			if b == '\'' {
				dst.Write([]byte{b})
				continue
			}
			dst.Write([]byte{'$', b})
			continue
		}
		if b == '$' {
			prevDollar = true
			continue
		}

		if b == 0x1b {
			inEscape = true
			continue
		}
		if inEscape {
			if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == 0x07 {
				inEscape = false
			}
			continue
		}

		dst.Write([]byte{b})
	}
}

func paintOutput(ptmx *os.File) {
	buf := make([]byte, 4096)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			os.Stdout.WriteString(colorOn)
			os.Stdout.Write(buf[:n])
			os.Stdout.WriteString(colorOff)
		}
		if err != nil {
			return
		}
	}
}

func runShell(rcPath string, cfg *Config, first bool) error {
	cmd := exec.Command("bash", "--rcfile", rcPath, "-i")
	cmd.Env = append(os.Environ(),
		"IGNOREEOF=999999",
		"AI_URL="+cfg.APIURL,
		"AI_KEY="+cfg.APIKey,
		"AI_MODEL="+cfg.Model,
	)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer ptmx.Close()

	if first {
		m := modes[loadMode()]
		os.Stdout.WriteString(colorOn + m.Phrases["greet"] + "\r\n" + colorOff)
	} else {
		os.Stdout.WriteString(colorOn + "Т-ты думал, я уйду? Б-бака.\r\n" + colorOff)
	}

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			pty.InheritSize(os.Stdin, ptmx)
		}
	}()
	ch <- syscall.SIGWINCH

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	go filterInput(ptmx)
	paintOutput(ptmx)
	return nil
}

func main() {
	cfg, err := ensureConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	mode := askMode()

	fmt.Printf("\n%sРежим: %s%s\n", colorOn, modes[mode].Name, colorOff)

	signal.Ignore(syscall.SIGTSTP, syscall.SIGQUIT)
	os.Remove("/tmp/baka_release")

	rcPath, err := writeRc(buildRc())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.Remove(rcPath)

	for i := 0; i < 50; i++ {
		if err := runShell(rcPath, cfg, i == 0); err == nil {
			break
		}
		if _, err := os.Stat("/tmp/baka_release"); err == nil {
			os.Remove("/tmp/baka_release")
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
}
