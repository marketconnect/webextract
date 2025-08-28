package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
	"github.com/joho/godotenv"
)

// ... (captchaKeywords, глобальные переменные и структуры остаются без изменений) ...
var captchaKeywords = []string{
	"капча", "не робот", "подозрительная активность", "подтвердите, что",
	"unusual traffic", "are you a robot", "prove you are human", "captcha",
}
var (
	persistentBrowserCtx context.Context
	isCaptchaPending     bool
	captchaMutex         sync.Mutex
)

type Link struct {
	Href string `json:"href"`
	Text string `json:"text"`
}
type Meta struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Keywords    string `json:"keywords"`
}
type Response struct {
	Content string `json:"content,omitempty"`
	Links   []Link `json:"links,omitempty"`
	Meta    *Meta  `json:"meta,omitempty"`
}
type ErrorResponse struct {
	Error string `json:"error"`
}

// ... (sendTelegramNotification и detectAndPauseOnCaptcha остаются без изменений) ...
func sendTelegramNotification(message string) {
	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatID := os.Getenv("TELEGRAM_CHAT_ID")
	if botToken == "" || chatID == "" {
		log.Println("ЛОГ: Переменные TELEGRAM не установлены, уведомление пропущено.")
		return
	}
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	requestBody, _ := json.Marshal(map[string]string{"chat_id": chatID, "text": message})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(apiURL, "application/json", bytes.NewBuffer(requestBody))
	if err != nil {
		log.Printf("ЛОГ: Ошибка отправки в Telegram: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		log.Println("ЛОГ: Уведомление в Telegram успешно отправлено.")
	} else {
		log.Printf("ЛОГ: Telegram API вернул ошибку: %s", resp.Status)
	}
}
func detectAndPauseOnCaptcha(url string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		log.Println("ЛОГ: Шаг [1] - Проверяю наличие CAPTCHA на странице.")
		var bodyText string
		if err := chromedp.Text(`body`, &bodyText, chromedp.ByQuery).Do(ctx); err != nil {
			return err
		}
		lowerBodyText := strings.ToLower(bodyText)
		for _, keyword := range captchaKeywords {
			if strings.Contains(lowerBodyText, keyword) {
				captchaMutex.Lock()
				isCaptchaPending = true
				captchaMutex.Unlock()
				message := fmt.Sprintf("🚨 ОБНАРУЖЕНА CAPTCHA! (Найдено слово: '%s') 🚨\n\nURL: %s\n\nДействие остановлено. Пожалуйста, решите капчу и нажмите Enter в этой консоли.", keyword, url)
				go sendTelegramNotification(message)
				log.Println("\n======================================================================")
				log.Println(message)
				log.Println("======================================================================")
				for {
					captchaMutex.Lock()
					if !isCaptchaPending {
						captchaMutex.Unlock()
						break
					}
					captchaMutex.Unlock()
					time.Sleep(1 * time.Second)
				}
				log.Println("ЛОГ: Enter нажат, продолжаю выполнение...")
				return chromedp.Sleep(2 * time.Second).Do(ctx)
			}
		}
		log.Println("ЛОГ: Шаг [1] - CAPTCHA не обнаружена, продолжаю.")
		return nil
	})
}

func writeJsonError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(ErrorResponse{Error: message})
}

func scrapeHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("\nЛОГ: Получен новый запрос: %s", r.URL.String())

	captchaMutex.Lock()
	if isCaptchaPending {
		captchaMutex.Unlock()
		log.Println("ЛОГ: Отклоняю запрос, так как уже решается CAPTCHA.")
		writeJsonError(w, "Сервис занят решением CAPTCHA. Попробуйте позже.", http.StatusServiceUnavailable)
		return
	}
	captchaMutex.Unlock()

	url := r.URL.Query().Get("url")
	if url == "" {
		writeJsonError(w, "Параметр 'url' обязателен", http.StatusBadRequest)
		return
	}

	tabCtx, cancelTab := chromedp.NewContext(persistentBrowserCtx)
	defer cancelTab()

	var response Response
	var tasks chromedp.Tasks

	tasks = append(tasks, chromedp.Navigate(url))
	tasks = append(tasks, chromedp.WaitVisible(`body`, chromedp.ByQuery))
	tasks = append(tasks, detectAndPauseOnCaptcha(url))

	// --- Временные переменные для безопасного сбора данных ---
	var (
		content   string
		meta      Meta
		descOK    bool // Флаг, что description найден
		keysOK    bool // Флаг, что keywords найден
		linkNodes []*cdp.Node
	)

	// --- Динамически строим ПЛОСКИЙ список задач ---
	if r.URL.Query().Has("content") {
		log.Println("ЛОГ: Добавляю в очередь задачу: сбор КОНТЕНТА.")
		tasks = append(tasks, chromedp.Text(`body`, &content, chromedp.ByQuery))
	}

	if r.URL.Query().Has("meta") {
		log.Println("ЛОГ: Добавляю в очередь задачу: сбор МЕТА-ДАННЫХ.")
		tasks = append(tasks,
			chromedp.Title(&meta.Title),
			// !!! ГЛАВНОЕ ИСПРАВЛЕНИЕ: Передаем указатели на `descOK` и `keysOK` !!!
			// Это делает поиск НЕБЛОКИРУЮЩИМ. Если тега нет, `ok` станет `false`, и мы пойдем дальше.
			chromedp.AttributeValue(`meta[name="description"]`, "content", &meta.Description, &descOK, chromedp.ByQuery),
			chromedp.AttributeValue(`meta[name="keywords"]`, "content", &meta.Keywords, &keysOK, chromedp.ByQuery),
		)
	}

	if r.URL.Query().Has("links") {
		log.Println("ЛОГ: Добавляю в очередь задачу: сбор ССЫЛОК.")
		tasks = append(tasks, chromedp.Nodes("a", &linkNodes, chromedp.ByQueryAll))
	}

	// --- Финальное действие: обработка всех собранных данных ---
	tasks = append(tasks, chromedp.ActionFunc(func(ctx context.Context) error {
		log.Println("ЛОГ: Шаг [2] - Обрабатываю собранные данные.")
		if r.URL.Query().Has("content") {
			response.Content = strings.TrimSpace(content)
		}
		if r.URL.Query().Has("meta") {
			response.Meta = &meta
		}
		if r.URL.Query().Has("links") {
			for _, node := range linkNodes {
				href := node.AttributeValue("href")
				if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
					continue
				}
				var text string
				_ = chromedp.TextContent(node.FullXPath(), &text, chromedp.BySearch).Do(ctx)
				response.Links = append(response.Links, Link{
					Href: href,
					Text: strings.TrimSpace(text),
				})
			}
		}
		return nil
	}))

	log.Println("ЛОГ: Шаг [0] - Начинаю выполнение всех задач.")
	if err := chromedp.Run(tabCtx, tasks); err != nil {
		log.Printf("ЛОГ: Ошибка во время выполнения chromedp: %v", err)
		writeJsonError(w, "Не удалось выполнить скрапинг: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Println("ЛОГ: Все задачи успешно выполнены.")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(response)
}

// ... (manageConsoleInput и main без изменений) ...
func manageConsoleInput() {
	reader := bufio.NewReader(os.Stdin)
	for {
		reader.ReadString('\n')
		captchaMutex.Lock()
		if isCaptchaPending {
			isCaptchaPending = false
			log.Println("ЛОГ: Консоль: получен Enter, флаг CAPTCHA снят.")
		}
		captchaMutex.Unlock()
	}
}

func main() {
	_ = godotenv.Load()
	headless := flag.Bool("headless", false, "Запуск браузера в headless режиме")
	flag.Parse()

	if *headless {
		log.Fatal("КРИТИЧЕСКАЯ ОШИБКА: Этот режим требует ручного ввода и не может работать с флагом -headless=true")
	}

	go manageConsoleInput()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", *headless),
		chromedp.UserAgent(`Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/117.0.0.0 Safari/537.36`),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.NoSandbox,
		chromedp.DisableGPU,
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()

	var cancelBrowser func()
	persistentBrowserCtx, cancelBrowser = chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
	defer cancelBrowser()

	if err := chromedp.Run(persistentBrowserCtx); err != nil {
		log.Fatalf("Не удалось запустить браузер: %v", err)
	}
	log.Println("ЛОГ: Постоянный экземпляр браузера успешно запущен.")

	http.HandleFunc("/scrape", scrapeHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("Сервер запущен на http://localhost%s", addr)
	log.Println("Режим: с графическим интерфейсом (non-headless)")
	log.Fatal(http.ListenAndServe(addr, nil))
}
