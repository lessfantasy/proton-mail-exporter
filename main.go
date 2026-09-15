package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	IMAPAddr     string
	IMAPUser     string
	IMAPPassword string
	ListenAddr   string
	Interval     time.Duration
	TLSInsecure  bool
}

type Exporter struct {
	cfg Config

	mu sync.RWMutex

	messages map[string]float64
	unread   map[string]float64

	lastSuccess float64
	lastScrape  float64
}

var (
	messagesDesc = prometheus.NewDesc(
		"proton_mail_messages",
		"Number of messages in a Proton Mail mailbox.",
		[]string{"mailbox"},
		nil,
	)

	unreadDesc = prometheus.NewDesc(
		"proton_mail_unread",
		"Number of unread messages in a Proton Mail mailbox.",
		[]string{"mailbox"},
		nil,
	)

	scrapeSuccessDesc = prometheus.NewDesc(
		"proton_mail_scrape_success",
		"1 if the last Proton Mail scrape succeeded, 0 otherwise.",
		nil,
		nil,
	)

	lastScrapeDesc = prometheus.NewDesc(
		"proton_mail_last_scrape_timestamp_seconds",
		"Unix timestamp of the last successful Proton Mail scrape.",
		nil,
		nil,
	)
)

func NewExporter(cfg Config) *Exporter {
	return &Exporter{
		cfg:      cfg,
		messages: make(map[string]float64),
		unread:   make(map[string]float64),
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- messagesDesc
	ch <- unreadDesc
	ch <- scrapeSuccessDesc
	ch <- lastScrapeDesc
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for mailbox, value := range e.messages {
		ch <- prometheus.MustNewConstMetric(
			messagesDesc,
			prometheus.GaugeValue,
			value,
			mailbox,
		)
	}

	for mailbox, value := range e.unread {
		ch <- prometheus.MustNewConstMetric(
			unreadDesc,
			prometheus.GaugeValue,
			value,
			mailbox,
		)
	}

	ch <- prometheus.MustNewConstMetric(
		scrapeSuccessDesc,
		prometheus.GaugeValue,
		e.lastSuccess,
	)

	ch <- prometheus.MustNewConstMetric(
		lastScrapeDesc,
		prometheus.GaugeValue,
		e.lastScrape,
	)
}

func (e *Exporter) Scrape() error {
	tlsConfig := &tls.Config{
		ServerName:         "127.0.0.1",
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: e.cfg.TLSInsecure, //nolint:gosec
	}

	client, err := imapclient.DialTLS(
		e.cfg.IMAPAddr,
		&imapclient.Options{
			TLSConfig: tlsConfig,
		},
	)
	if err != nil {
		return fmt.Errorf("connect to IMAP: %w", err)
	}
	defer client.Close()

	if err := client.Login(
		e.cfg.IMAPUser,
		e.cfg.IMAPPassword,
	).Wait(); err != nil {
		return fmt.Errorf("IMAP login: %w", err)
	}

	mailboxes, err := client.List("", "*", nil).Collect()
	if err != nil {
		return fmt.Errorf("LIST mailboxes: %w", err)
	}

	messages := make(map[string]float64)
	unread := make(map[string]float64)

	for _, mailbox := range mailboxes {
		if mailbox == nil || mailbox.Mailbox == "" {
			continue
		}

		name := mailbox.Mailbox

		status, err := client.Status(name, &imap.StatusOptions{
			NumMessages: true,
			NumUnseen:   true,
		}).Wait()

		if err != nil {
			log.Printf(
				"status failed for mailbox %q: %v",
				name,
				err,
			)
			continue
		}

		if status.NumMessages != nil {
			messages[name] = float64(*status.NumMessages)
		}

		if status.NumUnseen != nil {
			unread[name] = float64(*status.NumUnseen)
		}
	}

	now := float64(time.Now().Unix())

	e.mu.Lock()
	e.messages = messages
	e.unread = unread
	e.lastSuccess = 1
	e.lastScrape = now
	e.mu.Unlock()

	return nil
}

func (e *Exporter) Run() {
	ticker := time.NewTicker(e.cfg.Interval)
	defer ticker.Stop()

	for {
		if err := e.Scrape(); err != nil {
			log.Printf("scrape failed: %v", err)

			e.mu.Lock()
			e.lastSuccess = 0
			e.mu.Unlock()
		} else {
			log.Printf("scrape successful")
		}

		<-ticker.C
	}
}

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	return value
}

func getenvBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	v, err := strconv.ParseBool(value)
	if err != nil {
		log.Printf(
			"invalid boolean %s=%q, using %v",
			key,
			value,
			fallback,
		)
		return fallback
	}

	return v
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	v, err := time.ParseDuration(value)
	if err != nil {
		log.Printf(
			"invalid duration %s=%q, using %v",
			key,
			value,
			fallback,
		)
		return fallback
	}

	return v
}

func main() {
	cfg := Config{
		IMAPAddr:     getenv("PROTON_IMAP_ADDR", "127.0.0.1:1143"),
		IMAPUser:     os.Getenv("PROTON_IMAP_USER"),
		IMAPPassword: os.Getenv("PROTON_IMAP_PASSWORD"),
		ListenAddr:   getenv("LISTEN_ADDR", ":8080"),
		Interval:     getenvDuration("SCRAPE_INTERVAL", 60*time.Second),
		TLSInsecure:  getenvBool("PROTON_IMAP_TLS_INSECURE", true),
	}

	if cfg.IMAPUser == "" {
		log.Fatal("PROTON_IMAP_USER is required")
	}

	if cfg.IMAPPassword == "" {
		log.Fatal("PROTON_IMAP_PASSWORD is required")
	}

	exporter := NewExporter(cfg)

	registry := prometheus.NewRegistry()
	registry.MustRegister(exporter)

	http.Handle(
		"/metrics",
		promhttp.HandlerFor(
			registry,
			promhttp.HandlerOpts{},
		),
	)

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		exporter.mu.RLock()
		success := exporter.lastSuccess
		exporter.mu.RUnlock()

		if success != 1 {
			http.Error(
				w,
				"Proton Mail scrape failed",
				http.StatusServiceUnavailable,
			)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	log.Printf(
		"Proton Mail exporter listening on %s",
		cfg.ListenAddr,
	)
	log.Printf("IMAP: %s", cfg.IMAPAddr)
	log.Printf("scrape interval: %s", cfg.Interval)

	go exporter.Run()

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil &&
		!errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}