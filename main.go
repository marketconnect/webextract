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

// !!! НОВОЕ: Список ключевых слов для обнаружения CAPTCHA на разных сайтах !!!
var captchaKeywords = []string{
	// Русские
	"капча",
	"не робот",
	"подозрительная активность",
	"подтвердите, что",

	// Английские
	"unusual traffic",
	"are you a robot",
	"prove you are human",
	"captcha",
}

// --- Глобальное состояние для управления CAPTCHA ---
var (
	persistentBrowserCtx context.Context
	isCaptchaPending     bool
	captchaMutex         sync.Mutex
)

// ... (структуры Link, Meta, Response без изменений) ...
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

// ... (sendTelegramNotification без изменений) ...
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

// detectAndPauseOnCaptcha - теперь проверяет по списку ключевых слов.
func detectAndPauseOnCaptcha(url string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		log.Println("ЛОГ: Шаг [1] - Проверяю наличие CAPTCHA на странице.")
		var bodyText string
		if err := chromedp.Text(`body`, &bodyText, chromedp.ByQuery).Do(ctx); err != nil {
			return err
		}

		// Приводим текст страницы к нижнему регистру для надежного поиска
		lowerBodyText := strings.ToLower(bodyText)

		// !!! ИЗМЕНЕННАЯ ЛОГИКА: Ищем любое из ключевых слов !!!
		for _, keyword := range captchaKeywords {
			if strings.Contains(lowerBodyText, keyword) {
				// Нашли! Запускаем процедуру ожидания.
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

func scrapeHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("\nЛОГ: Получен новый запрос: %s", r.URL.String())

	captchaMutex.Lock()
	if isCaptchaPending {
		captchaMutex.Unlock()
		log.Println("ЛОГ: Отклоняю запрос, так как уже решается CAPTCHA.")
		http.Error(w, "Сервис занят решением CAPTCHA. Попробуйте позже.", http.StatusServiceUnavailable)
		return
	}
	captchaMutex.Unlock()

	url := r.URL.Query().Get("url")
	if url == "" {
		http.Error(w, "Параметр 'url' обязателен", http.StatusBadRequest)
		return
	}

	tabCtx, cancelTab := chromedp.NewContext(persistentBrowserCtx)
	defer func() {
		log.Println("ЛОГ: Шаг [4] - Обработчик завершен, закрываю вкладку.")
		cancelTab()
	}()

	var response Response

	log.Println("ЛОГ: Шаг [0] - Начинаю выполнение задач в новой вкладке.")
	err := chromedp.Run(tabCtx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`body`, chromedp.ByQuery),
		detectAndPauseOnCaptcha(url),
		chromedp.ActionFunc(func(c context.Context) error {
			log.Println("ЛОГ: Шаг [2] - Собираю данные со страницы.")
			// --- Полная логика сбора данных ---
			if r.URL.Query().Has("content") {
				var content string
				if err := chromedp.Text(`body`, &content, chromedp.ByQuery).Do(c); err != nil {
					return err
				}
				response.Content = strings.TrimSpace(content)
			}
			if r.URL.Query().Has("links") {
				var nodes []*cdp.Node
				if err := chromedp.Nodes("a", &nodes, chromedp.ByQueryAll).Do(c); err != nil {
					return err
				}
				for _, node := range nodes {
					href := node.AttributeValue("href")
					if href == "" || strings.HasPrefix(href, "#") {
						continue
					}
					var text string
					// Используем Do(c), чтобы выполнить действие в текущем контексте вкладки
					_ = chromedp.Text(node.FullXPath(), &text, chromedp.BySearch).Do(c)
					response.Links = append(response.Links, Link{
						Href: href,
						Text: strings.TrimSpace(text),
					})
				}
			}
			if r.URL.Query().Has("meta") {
				var meta Meta
				_ = chromedp.Title(&meta.Title).Do(c)
				_ = chromedp.AttributeValue(`meta[name="description"]`, "content", &meta.Description, nil, chromedp.ByQuery).Do(c)
				_ = chromedp.AttributeValue(`meta[name="keywords"]`, "content", &meta.Keywords, nil, chromedp.ByQuery).Do(c)
				response.Meta = &meta
			}
			// --- Конец логики сбора данных ---
			log.Println("ЛОГ: Шаг [3] - Сбор данных завершен.")
			return nil
		}),
	)

	if err != nil {
		log.Printf("ЛОГ: Ошибка во время выполнения chromedp: %v", err)
		http.Error(w, "Не удалось выполнить скрапинг: "+err.Error(), http.StatusInternalServerError)
		return
	}

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
