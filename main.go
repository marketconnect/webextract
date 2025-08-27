package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
)

// ... (структуры Link, Meta, Response остаются без изменений) ...
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

func scrapeHandler(allocCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		url := r.URL.Query().Get("url")
		if url == "" {
			http.Error(w, "Необходимо указать параметр 'url'", http.StatusBadRequest)
			return
		}

		ctx, cancel := chromedp.NewContext(allocCtx)
		defer cancel()
		ctx, cancel = context.WithTimeout(ctx, 25*time.Second) // Увеличим общий таймаут
		defer cancel()

		var response Response
		var tasks chromedp.Tasks

		tasks = append(tasks, chromedp.Navigate(url))
		tasks = append(tasks, chromedp.WaitVisible(`body`, chromedp.ByQuery))
		// !!! ДОБАВЛЕНО: Небольшая задержка для имитации поведения пользователя
		tasks = append(tasks, chromedp.Sleep(2*time.Second))

		// ... (логика для content, links, meta остается без изменений) ...
		if r.URL.Query().Has("content") {
			var content string
			tasks = append(tasks, chromedp.Text(`body`, &content, chromedp.ByQuery))
			tasks = append(tasks, chromedp.ActionFunc(func(c context.Context) error {
				response.Content = strings.TrimSpace(content)
				return nil
			}))
		}
		if r.URL.Query().Has("links") {
			var nodes []*cdp.Node
			tasks = append(tasks, chromedp.Nodes("a", &nodes, chromedp.ByQueryAll))
			tasks = append(tasks, chromedp.ActionFunc(func(ctx context.Context) error {
				taskCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				for _, node := range nodes {
					href := node.AttributeValue("href")
					if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
						continue
					}
					var text string
					err := chromedp.Run(taskCtx, chromedp.Text(node.FullXPath(), &text, chromedp.BySearch))
					if err == nil && strings.TrimSpace(text) != "" {
						response.Links = append(response.Links, Link{
							Href: href,
							Text: strings.TrimSpace(text),
						})
					}
				}
				return nil
			}))
		}
		if r.URL.Query().Has("meta") {
			var meta Meta
			tasks = append(tasks, chromedp.Title(&meta.Title))
			tasks = append(tasks, chromedp.AttributeValue(`meta[name="description"]`, "content", &meta.Description, nil, chromedp.ByQuery))
			tasks = append(tasks, chromedp.AttributeValue(`meta[name="keywords"]`, "content", &meta.Keywords, nil, chromedp.ByQuery))
			tasks = append(tasks, chromedp.ActionFunc(func(c context.Context) error {
				response.Meta = &meta
				return nil
			}))
		}

		if err := chromedp.Run(ctx, tasks); err != nil {
			log.Printf("Ошибка при выполнении скрапинга для URL %s: %v", url, err)
			http.Error(w, "Не удалось выполнить скрапинг: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(response)
	}
}

func main() {
	headless := flag.Bool("headless", true, "Запуск браузера в headless режиме")
	flag.Parse()

	// !!! ИЗМЕНЕНО: Добавляем опции для маскировки
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", *headless),
		// Устанавливаем "человеческий" User-Agent
		chromedp.UserAgent(`Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/117.0.0.0 Safari/537.36`),
		// Отключаем флаги, которые выдают автоматизацию
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.NoSandbox,
		chromedp.DisableGPU,
	)

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	// Создаем контекст для логгирования, чтобы видеть сообщения от браузера (полезно для отладки)
	logCtx, cancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
	defer cancel()

	// Убедимся, что браузер запущен, выполнив пустое действие
	if err := chromedp.Run(logCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return nil
	})); err != nil {
		log.Fatalf("Не удалось запустить браузер: %v", err)
	}

	http.HandleFunc("/scrape", scrapeHandler(logCtx)) // Используем контекст с логгером

	log.Println("Сервер запущен на http://localhost:8080")
	if *headless {
		log.Println("Режим: headless")
	} else {
		log.Println("Режим: с графическим интерфейсом (non-headless)")
	}

	log.Fatal(http.ListenAndServe(":8080", nil))
}
