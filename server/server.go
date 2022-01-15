package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	firebase "firebase.google.com/go"
	"firebase.google.com/go/messaging"
	"fmt"
	"github.com/emersion/go-smtp"
	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/option"
	"heckel.io/ntfy/util"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// TODO add "max messages in a topic" limit
// TODO implement "since=<ID>"

// Server is the main server, providing the UI and API for ntfy
type Server struct {
	config       *Config
	httpServer   *http.Server
	httpsServer  *http.Server
	unixListener net.Listener
	smtpServer   *smtp.Server
	smtpBackend  *smtpBackend
	topics       map[string]*topic
	visitors     map[string]*visitor
	firebase     subscriber
	mailer       mailer
	messages     int64
	cache        cache
	fileCache    *fileCache
	closeChan    chan bool
	mu           sync.Mutex
}

// errHTTP is a generic HTTP error for any non-200 HTTP error
type errHTTP struct {
	Code     int    `json:"code,omitempty"`
	HTTPCode int    `json:"http"`
	Message  string `json:"error"`
	Link     string `json:"link,omitempty"`
}

func (e errHTTP) Error() string {
	return e.Message
}

func (e errHTTP) JSON() string {
	b, _ := json.Marshal(&e)
	return string(b)
}

type indexPage struct {
	Topic         string
	CacheDuration time.Duration
}

type sinceTime time.Time

func (t sinceTime) IsAll() bool {
	return t == sinceAllMessages
}

func (t sinceTime) IsNone() bool {
	return t == sinceNoMessages
}

func (t sinceTime) Time() time.Time {
	return time.Time(t)
}

var (
	sinceAllMessages = sinceTime(time.Unix(0, 0))
	sinceNoMessages  = sinceTime(time.Unix(1, 0))
)

var (
	topicRegex       = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)  // No /!
	topicPathRegex   = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}$`) // Regex must match JS & Android app!
	jsonPathRegex    = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/json$`)
	ssePathRegex     = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/sse$`)
	rawPathRegex     = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/raw$`)
	wsPathRegex      = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/ws$`)
	publishPathRegex = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/(publish|send|trigger)$`)

	staticRegex      = regexp.MustCompile(`^/static/.+`)
	docsRegex        = regexp.MustCompile(`^/docs(|/.*)$`)
	fileRegex        = regexp.MustCompile(`^/file/([-_A-Za-z0-9]{1,64})(?:\.[A-Za-z0-9]{1,16})?$`)
	disallowedTopics = []string{"docs", "static", "file"}
	attachURLRegex   = regexp.MustCompile(`^https?://`)

	templateFnMap = template.FuncMap{
		"durationToHuman": util.DurationToHuman,
	}

	//go:embed "index.gohtml"
	indexSource   string
	indexTemplate = template.Must(template.New("index").Funcs(templateFnMap).Parse(indexSource))

	//go:embed "example.html"
	exampleSource string

	//go:embed static
	webStaticFs       embed.FS
	webStaticFsCached = &util.CachingEmbedFS{ModTime: time.Now(), FS: webStaticFs}

	//go:embed docs
	docsStaticFs     embed.FS
	docsStaticCached = &util.CachingEmbedFS{ModTime: time.Now(), FS: docsStaticFs}

	errHTTPBadRequestEmailDisabled                   = &errHTTP{40001, http.StatusBadRequest, "e-mail notifications are not enabled", "https://ntfy.sh/docs/config/#e-mail-notifications"}
	errHTTPBadRequestDelayNoCache                    = &errHTTP{40002, http.StatusBadRequest, "cannot disable cache for delayed message", ""}
	errHTTPBadRequestDelayNoEmail                    = &errHTTP{40003, http.StatusBadRequest, "delayed e-mail notifications are not supported", ""}
	errHTTPBadRequestDelayCannotParse                = &errHTTP{40004, http.StatusBadRequest, "invalid delay parameter: unable to parse delay", "https://ntfy.sh/docs/publish/#scheduled-delivery"}
	errHTTPBadRequestDelayTooSmall                   = &errHTTP{40005, http.StatusBadRequest, "invalid delay parameter: too small, please refer to the docs", "https://ntfy.sh/docs/publish/#scheduled-delivery"}
	errHTTPBadRequestDelayTooLarge                   = &errHTTP{40006, http.StatusBadRequest, "invalid delay parameter: too large, please refer to the docs", "https://ntfy.sh/docs/publish/#scheduled-delivery"}
	errHTTPBadRequestPriorityInvalid                 = &errHTTP{40007, http.StatusBadRequest, "invalid priority parameter", "https://ntfy.sh/docs/publish/#message-priority"}
	errHTTPBadRequestSinceInvalid                    = &errHTTP{40008, http.StatusBadRequest, "invalid since parameter", "https://ntfy.sh/docs/subscribe/api/#fetch-cached-messages"}
	errHTTPBadRequestTopicInvalid                    = &errHTTP{40009, http.StatusBadRequest, "invalid topic: path invalid", ""}
	errHTTPBadRequestTopicDisallowed                 = &errHTTP{40010, http.StatusBadRequest, "invalid topic: topic name is disallowed", ""}
	errHTTPBadRequestMessageNotUTF8                  = &errHTTP{40011, http.StatusBadRequest, "invalid message: message must be UTF-8 encoded", ""}
	errHTTPBadRequestAttachmentTooLarge              = &errHTTP{40012, http.StatusBadRequest, "invalid request: attachment too large, or bandwidth limit reached", ""}
	errHTTPBadRequestAttachmentURLInvalid            = &errHTTP{40013, http.StatusBadRequest, "invalid request: attachment URL is invalid", ""}
	errHTTPBadRequestAttachmentsDisallowed           = &errHTTP{40014, http.StatusBadRequest, "invalid request: attachments not allowed", ""}
	errHTTPBadRequestAttachmentsExpiryBeforeDelivery = &errHTTP{40015, http.StatusBadRequest, "invalid request: attachment expiry before delayed delivery date", ""}
	errHTTPNotFound                                  = &errHTTP{40401, http.StatusNotFound, "page not found", ""}
	errHTTPTooManyRequestsLimitRequests              = &errHTTP{42901, http.StatusTooManyRequests, "limit reached: too many requests, please be nice", "https://ntfy.sh/docs/publish/#limitations"}
	errHTTPTooManyRequestsLimitEmails                = &errHTTP{42902, http.StatusTooManyRequests, "limit reached: too many emails, please be nice", "https://ntfy.sh/docs/publish/#limitations"}
	errHTTPTooManyRequestsLimitSubscriptions         = &errHTTP{42903, http.StatusTooManyRequests, "limit reached: too many active subscriptions, please be nice", "https://ntfy.sh/docs/publish/#limitations"}
	errHTTPTooManyRequestsLimitTotalTopics           = &errHTTP{42904, http.StatusTooManyRequests, "limit reached: the total number of topics on the server has been reached, please contact the admin", "https://ntfy.sh/docs/publish/#limitations"}
	errHTTPTooManyRequestsAttachmentBandwidthLimit   = &errHTTP{42905, http.StatusTooManyRequests, "too many requests: daily bandwidth limit reached", "https://ntfy.sh/docs/publish/#limitations"}
	errHTTPInternalError                             = &errHTTP{50001, http.StatusInternalServerError, "internal server error", ""}
	errHTTPInternalErrorInvalidFilePath              = &errHTTP{50002, http.StatusInternalServerError, "internal server error: invalid file path", ""}
)

const (
	firebaseControlTopic     = "~control"                // See Android if changed
	emptyMessageBody         = "triggered"               // Used if message body is empty
	defaultAttachmentMessage = "You received a file: %s" // Used if message body is empty, and there is an attachment
	fcmMessageLimit          = 4000                      // see maybeTruncateFCMMessage for details
	wsWriteWait              = 2 * time.Second
	wsBufferSize             = 1024
	wsReadLimit              = 64 // We only ever receive PINGs
	wsPongWait               = 15 * time.Second
)

// New instantiates a new Server. It creates the cache and adds a Firebase
// subscriber (if configured).
func New(conf *Config) (*Server, error) {
	var firebaseSubscriber subscriber
	if conf.FirebaseKeyFile != "" {
		var err error
		firebaseSubscriber, err = createFirebaseSubscriber(conf)
		if err != nil {
			return nil, err
		}
	}
	var mailer mailer
	if conf.SMTPSenderAddr != "" {
		mailer = &smtpSender{config: conf}
	}
	cache, err := createCache(conf)
	if err != nil {
		return nil, err
	}
	topics, err := cache.Topics()
	if err != nil {
		return nil, err
	}
	var fileCache *fileCache
	if conf.AttachmentCacheDir != "" {
		fileCache, err = newFileCache(conf.AttachmentCacheDir, conf.AttachmentTotalSizeLimit, conf.AttachmentFileSizeLimit)
		if err != nil {
			return nil, err
		}
	}
	return &Server{
		config:    conf,
		cache:     cache,
		fileCache: fileCache,
		firebase:  firebaseSubscriber,
		mailer:    mailer,
		topics:    topics,
		visitors:  make(map[string]*visitor),
	}, nil
}

func createCache(conf *Config) (cache, error) {
	if conf.CacheDuration == 0 {
		return newNopCache(), nil
	} else if conf.CacheFile != "" {
		return newSqliteCache(conf.CacheFile)
	}
	return newMemCache(), nil
}

func createFirebaseSubscriber(conf *Config) (subscriber, error) {
	fb, err := firebase.NewApp(context.Background(), nil, option.WithCredentialsFile(conf.FirebaseKeyFile))
	if err != nil {
		return nil, err
	}
	msg, err := fb.Messaging(context.Background())
	if err != nil {
		return nil, err
	}
	return func(m *message) error {
		var data map[string]string // Matches https://ntfy.sh/docs/subscribe/api/#json-message-format
		switch m.Event {
		case keepaliveEvent, openEvent:
			data = map[string]string{
				"id":    m.ID,
				"time":  fmt.Sprintf("%d", m.Time),
				"event": m.Event,
				"topic": m.Topic,
			}
		case messageEvent:
			data = map[string]string{
				"id":       m.ID,
				"time":     fmt.Sprintf("%d", m.Time),
				"event":    m.Event,
				"topic":    m.Topic,
				"priority": fmt.Sprintf("%d", m.Priority),
				"tags":     strings.Join(m.Tags, ","),
				"click":    m.Click,
				"title":    m.Title,
				"message":  m.Message,
			}
			if m.Attachment != nil {
				data["attachment_name"] = m.Attachment.Name
				data["attachment_type"] = m.Attachment.Type
				data["attachment_size"] = fmt.Sprintf("%d", m.Attachment.Size)
				data["attachment_expires"] = fmt.Sprintf("%d", m.Attachment.Expires)
				data["attachment_url"] = m.Attachment.URL
			}
		}
		var androidConfig *messaging.AndroidConfig
		if m.Priority >= 4 {
			androidConfig = &messaging.AndroidConfig{
				Priority: "high",
			}
		}
		_, err := msg.Send(context.Background(), maybeTruncateFCMMessage(&messaging.Message{
			Topic:   m.Topic,
			Data:    data,
			Android: androidConfig,
		}))
		return err
	}, nil
}

// maybeTruncateFCMMessage performs best-effort truncation of FCM messages.
// The docs say the limit is 4000 characters, but during testing it wasn't quite clear
// what fields matter; so we're just capping the serialized JSON to 4000 bytes.
func maybeTruncateFCMMessage(m *messaging.Message) *messaging.Message {
	s, err := json.Marshal(m)
	if err != nil {
		return m
	}
	if len(s) > fcmMessageLimit {
		over := len(s) - fcmMessageLimit + 16 // = len("truncated":"1",), sigh ...
		message, ok := m.Data["message"]
		if ok && len(message) > over {
			m.Data["truncated"] = "1"
			m.Data["message"] = message[:len(message)-over]
		}
	}
	return m
}

// Run executes the main server. It listens on HTTP (+ HTTPS, if configured), and starts
// a manager go routine to print stats and prune messages.
func (s *Server) Run() error {
	var listenStr string
	if s.config.ListenHTTP != "" {
		listenStr += fmt.Sprintf(" %s[http]", s.config.ListenHTTP)
	}
	if s.config.ListenHTTPS != "" {
		listenStr += fmt.Sprintf(" %s[https]", s.config.ListenHTTPS)
	}
	if s.config.ListenUnix != "" {
		listenStr += fmt.Sprintf(" %s[unix]", s.config.ListenUnix)
	}
	if s.config.SMTPServerListen != "" {
		listenStr += fmt.Sprintf(" %s[smtp]", s.config.SMTPServerListen)
	}
	log.Printf("Listening on%s", listenStr)
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	errChan := make(chan error)
	s.mu.Lock()
	s.closeChan = make(chan bool)
	if s.config.ListenHTTP != "" {
		s.httpServer = &http.Server{Addr: s.config.ListenHTTP, Handler: mux}
		go func() {
			errChan <- s.httpServer.ListenAndServe()
		}()
	}
	if s.config.ListenHTTPS != "" {
		s.httpsServer = &http.Server{Addr: s.config.ListenHTTPS, Handler: mux}
		go func() {
			errChan <- s.httpsServer.ListenAndServeTLS(s.config.CertFile, s.config.KeyFile)
		}()
	}
	if s.config.ListenUnix != "" {
		go func() {
			var err error
			s.mu.Lock()
			os.Remove(s.config.ListenUnix)
			s.unixListener, err = net.Listen("unix", s.config.ListenUnix)
			if err != nil {
				errChan <- err
				return
			}
			s.mu.Unlock()
			httpServer := &http.Server{Handler: mux}
			errChan <- httpServer.Serve(s.unixListener)
		}()
	}
	if s.config.SMTPServerListen != "" {
		go func() {
			errChan <- s.runSMTPServer()
		}()
	}
	s.mu.Unlock()
	go s.runManager()
	go s.runAtSender()
	go s.runFirebaseKeepliver()

	return <-errChan
}

// Stop stops HTTP (+HTTPS) server and all managers
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpServer != nil {
		s.httpServer.Close()
	}
	if s.httpsServer != nil {
		s.httpsServer.Close()
	}
	if s.unixListener != nil {
		s.unixListener.Close()
	}
	if s.smtpServer != nil {
		s.smtpServer.Close()
	}
	close(s.closeChan)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if err := s.handleInternal(w, r); err != nil {
		var e *errHTTP
		var ok bool
		if e, ok = err.(*errHTTP); !ok {
			e = errHTTPInternalError
		}
		log.Printf("[%s] %s - %d - %d - %s", r.RemoteAddr, r.Method, e.HTTPCode, e.Code, err.Error())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
		w.WriteHeader(e.HTTPCode)
		io.WriteString(w, e.JSON()+"\n")
	}
}

func (s *Server) handleInternal(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		return s.handleHome(w, r)
	} else if r.Method == http.MethodGet && r.URL.Path == "/example.html" {
		return s.handleExample(w, r)
	} else if r.Method == http.MethodHead && r.URL.Path == "/" {
		return s.handleEmpty(w, r)
	} else if r.Method == http.MethodGet && staticRegex.MatchString(r.URL.Path) {
		return s.handleStatic(w, r)
	} else if r.Method == http.MethodGet && docsRegex.MatchString(r.URL.Path) {
		return s.handleDocs(w, r)
	} else if r.Method == http.MethodGet && fileRegex.MatchString(r.URL.Path) && s.config.AttachmentCacheDir != "" {
		return s.withRateLimit(w, r, s.handleFile)
	} else if r.Method == http.MethodOptions {
		return s.handleOptions(w, r)
	} else if r.Method == http.MethodGet && topicPathRegex.MatchString(r.URL.Path) {
		return s.handleTopic(w, r)
	} else if (r.Method == http.MethodPut || r.Method == http.MethodPost) && topicPathRegex.MatchString(r.URL.Path) {
		return s.withRateLimit(w, r, s.handlePublish)
	} else if r.Method == http.MethodGet && publishPathRegex.MatchString(r.URL.Path) {
		return s.withRateLimit(w, r, s.handlePublish)
	} else if r.Method == http.MethodGet && jsonPathRegex.MatchString(r.URL.Path) {
		return s.withRateLimit(w, r, s.handleSubscribeJSON)
	} else if r.Method == http.MethodGet && ssePathRegex.MatchString(r.URL.Path) {
		return s.withRateLimit(w, r, s.handleSubscribeSSE)
	} else if r.Method == http.MethodGet && rawPathRegex.MatchString(r.URL.Path) {
		return s.withRateLimit(w, r, s.handleSubscribeRaw)
	} else if r.Method == http.MethodGet && wsPathRegex.MatchString(r.URL.Path) {
		return s.withRateLimit(w, r, s.handleSubscribeWS)
	}
	return errHTTPNotFound
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) error {
	return indexTemplate.Execute(w, &indexPage{
		Topic:         r.URL.Path[1:],
		CacheDuration: s.config.CacheDuration,
	})
}

func (s *Server) handleTopic(w http.ResponseWriter, r *http.Request) error {
	unifiedpush := readParam(r, "x-unifiedpush", "unifiedpush", "up") == "1" // see PUT/POST too!
	if unifiedpush {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
		_, err := io.WriteString(w, `{"unifiedpush":{"version":1}}`+"\n")
		return err
	}
	return s.handleHome(w, r)
}

func (s *Server) handleEmpty(_ http.ResponseWriter, _ *http.Request) error {
	return nil
}

func (s *Server) handleExample(w http.ResponseWriter, _ *http.Request) error {
	_, err := io.WriteString(w, exampleSource)
	return err
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) error {
	http.FileServer(http.FS(webStaticFsCached)).ServeHTTP(w, r)
	return nil
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) error {
	http.FileServer(http.FS(docsStaticCached)).ServeHTTP(w, r)
	return nil
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request, v *visitor) error {
	if s.config.AttachmentCacheDir == "" {
		return errHTTPInternalError
	}
	matches := fileRegex.FindStringSubmatch(r.URL.Path)
	if len(matches) != 2 {
		return errHTTPInternalErrorInvalidFilePath
	}
	messageID := matches[1]
	file := filepath.Join(s.config.AttachmentCacheDir, messageID)
	stat, err := os.Stat(file)
	if err != nil {
		return errHTTPNotFound
	}
	if err := v.BandwidthLimiter().Allow(stat.Size()); err != nil {
		return errHTTPTooManyRequestsAttachmentBandwidthLimit
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(util.NewContentTypeWriter(w, r.URL.Path), f)
	return err
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request, v *visitor) error {
	t, err := s.topicFromPath(r.URL.Path)
	if err != nil {
		return err
	}
	body, err := util.Peak(r.Body, s.config.MessageLimit)
	if err != nil {
		return err
	}
	m := newDefaultMessage(t.ID, "")
	cache, firebase, email, err := s.parsePublishParams(r, v, m)
	if err != nil {
		return err
	}
	if err := s.handlePublishBody(r, v, m, body); err != nil {
		return err
	}
	if m.Message == "" {
		m.Message = emptyMessageBody
	}
	delayed := m.Time > time.Now().Unix()
	if !delayed {
		if err := t.Publish(m); err != nil {
			return err
		}
	}
	if s.firebase != nil && firebase && !delayed {
		go func() {
			if err := s.firebase(m); err != nil {
				log.Printf("Unable to publish to Firebase: %v", err.Error())
			}
		}()
	}
	if s.mailer != nil && email != "" && !delayed {
		go func() {
			if err := s.mailer.Send(v.ip, email, m); err != nil {
				log.Printf("Unable to send email: %v", err.Error())
			}
		}()
	}
	if cache {
		if err := s.cache.AddMessage(m); err != nil {
			return err
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
	if err := json.NewEncoder(w).Encode(m); err != nil {
		return err
	}
	s.inc(&s.messages)
	return nil
}

func (s *Server) parsePublishParams(r *http.Request, v *visitor, m *message) (cache bool, firebase bool, email string, err error) {
	cache = readParam(r, "x-cache", "cache") != "no"
	firebase = readParam(r, "x-firebase", "firebase") != "no"
	m.Title = readParam(r, "x-title", "title", "t")
	m.Click = readParam(r, "x-click", "click")
	filename := readParam(r, "x-filename", "filename", "file", "f")
	attach := readParam(r, "x-attach", "attach", "a")
	if attach != "" || filename != "" {
		m.Attachment = &attachment{}
	}
	if filename != "" {
		m.Attachment.Name = filename
	}
	if attach != "" {
		if !attachURLRegex.MatchString(attach) {
			return false, false, "", errHTTPBadRequestAttachmentURLInvalid
		}
		m.Attachment.URL = attach
		if m.Attachment.Name == "" {
			u, err := url.Parse(m.Attachment.URL)
			if err == nil {
				m.Attachment.Name = path.Base(u.Path)
				if m.Attachment.Name == "." || m.Attachment.Name == "/" {
					m.Attachment.Name = ""
				}
			}
		}
		if m.Attachment.Name == "" {
			m.Attachment.Name = "attachment"
		}
	}
	email = readParam(r, "x-email", "x-e-mail", "email", "e-mail", "mail", "e")
	if email != "" {
		if err := v.EmailAllowed(); err != nil {
			return false, false, "", errHTTPTooManyRequestsLimitEmails
		}
	}
	if s.mailer == nil && email != "" {
		return false, false, "", errHTTPBadRequestEmailDisabled
	}
	messageStr := readParam(r, "x-message", "message", "m")
	if messageStr != "" {
		m.Message = messageStr
	}
	m.Priority, err = util.ParsePriority(readParam(r, "x-priority", "priority", "prio", "p"))
	if err != nil {
		return false, false, "", errHTTPBadRequestPriorityInvalid
	}
	tagsStr := readParam(r, "x-tags", "tags", "tag", "ta")
	if tagsStr != "" {
		m.Tags = make([]string, 0)
		for _, s := range util.SplitNoEmpty(tagsStr, ",") {
			m.Tags = append(m.Tags, strings.TrimSpace(s))
		}
	}
	delayStr := readParam(r, "x-delay", "delay", "x-at", "at", "x-in", "in")
	if delayStr != "" {
		if !cache {
			return false, false, "", errHTTPBadRequestDelayNoCache
		}
		if email != "" {
			return false, false, "", errHTTPBadRequestDelayNoEmail // we cannot store the email address (yet)
		}
		delay, err := util.ParseFutureTime(delayStr, time.Now())
		if err != nil {
			return false, false, "", errHTTPBadRequestDelayCannotParse
		} else if delay.Unix() < time.Now().Add(s.config.MinDelay).Unix() {
			return false, false, "", errHTTPBadRequestDelayTooSmall
		} else if delay.Unix() > time.Now().Add(s.config.MaxDelay).Unix() {
			return false, false, "", errHTTPBadRequestDelayTooLarge
		}
		m.Time = delay.Unix()
	}
	unifiedpush := readParam(r, "x-unifiedpush", "unifiedpush", "up") == "1" // see GET too!
	if unifiedpush {
		firebase = false
	}
	return cache, firebase, email, nil
}

func readParam(r *http.Request, names ...string) string {
	for _, name := range names {
		value := r.Header.Get(name)
		if value != "" {
			return strings.TrimSpace(value)
		}
	}
	for _, name := range names {
		value := r.URL.Query().Get(strings.ToLower(name))
		if value != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// handlePublishBody consumes the PUT/POST body and decides whether the body is an attachment or the message.
//
// 1. curl -H "Attach: http://example.com/file.jpg" ntfy.sh/mytopic
//    Body must be a message, because we attached an external URL
// 2. curl -T short.txt -H "Filename: short.txt" ntfy.sh/mytopic
//    Body must be attachment, because we passed a filename
// 3. curl -T file.txt ntfy.sh/mytopic
//    If file.txt is <= 4096 (message limit) and valid UTF-8, treat it as a message
// 4. curl -T file.txt ntfy.sh/mytopic
//    If file.txt is > message limit, treat it as an attachment
func (s *Server) handlePublishBody(r *http.Request, v *visitor, m *message, body *util.PeakedReadCloser) error {
	if m.Attachment != nil && m.Attachment.URL != "" {
		return s.handleBodyAsMessage(m, body) // Case 1
	} else if m.Attachment != nil && m.Attachment.Name != "" {
		return s.handleBodyAsAttachment(r, v, m, body) // Case 2
	} else if !body.LimitReached && utf8.Valid(body.PeakedBytes) {
		return s.handleBodyAsMessage(m, body) // Case 3
	}
	return s.handleBodyAsAttachment(r, v, m, body) // Case 4
}

func (s *Server) handleBodyAsMessage(m *message, body *util.PeakedReadCloser) error {
	if !utf8.Valid(body.PeakedBytes) {
		return errHTTPBadRequestMessageNotUTF8
	}
	if len(body.PeakedBytes) > 0 { // Empty body should not override message (publish via GET!)
		m.Message = strings.TrimSpace(string(body.PeakedBytes)) // Truncates the message to the peak limit if required
	}
	if m.Attachment != nil && m.Attachment.Name != "" && m.Message == "" {
		m.Message = fmt.Sprintf(defaultAttachmentMessage, m.Attachment.Name)
	}
	return nil
}

func (s *Server) handleBodyAsAttachment(r *http.Request, v *visitor, m *message, body *util.PeakedReadCloser) error {
	if s.fileCache == nil || s.config.BaseURL == "" || s.config.AttachmentCacheDir == "" {
		return errHTTPBadRequestAttachmentsDisallowed
	} else if m.Time > time.Now().Add(s.config.AttachmentExpiryDuration).Unix() {
		return errHTTPBadRequestAttachmentsExpiryBeforeDelivery
	}
	visitorAttachmentsSize, err := s.cache.AttachmentsSize(v.ip)
	if err != nil {
		return err
	}
	remainingVisitorAttachmentSize := s.config.VisitorAttachmentTotalSizeLimit - visitorAttachmentsSize
	contentLengthStr := r.Header.Get("Content-Length")
	if contentLengthStr != "" { // Early "do-not-trust" check, hard limit see below
		contentLength, err := strconv.ParseInt(contentLengthStr, 10, 64)
		if err == nil && (contentLength > remainingVisitorAttachmentSize || contentLength > s.config.AttachmentFileSizeLimit) {
			return errHTTPBadRequestAttachmentTooLarge
		}
	}
	if m.Attachment == nil {
		m.Attachment = &attachment{}
	}
	var ext string
	m.Attachment.Owner = v.ip // Important for attachment rate limiting
	m.Attachment.Expires = time.Now().Add(s.config.AttachmentExpiryDuration).Unix()
	m.Attachment.Type, ext = util.DetectContentType(body.PeakedBytes, m.Attachment.Name)
	m.Attachment.URL = fmt.Sprintf("%s/file/%s%s", s.config.BaseURL, m.ID, ext)
	if m.Attachment.Name == "" {
		m.Attachment.Name = fmt.Sprintf("attachment%s", ext)
	}
	if m.Message == "" {
		m.Message = fmt.Sprintf(defaultAttachmentMessage, m.Attachment.Name)
	}
	m.Attachment.Size, err = s.fileCache.Write(m.ID, body, v.BandwidthLimiter(), util.NewFixedLimiter(remainingVisitorAttachmentSize))
	if err == util.ErrLimitReached {
		return errHTTPBadRequestAttachmentTooLarge
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Server) handleSubscribeJSON(w http.ResponseWriter, r *http.Request, v *visitor) error {
	encoder := func(msg *message) (string, error) {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(&msg); err != nil {
			return "", err
		}
		return buf.String(), nil
	}
	return s.handleSubscribe(w, r, v, "json", "application/x-ndjson", encoder)
}

func (s *Server) handleSubscribeSSE(w http.ResponseWriter, r *http.Request, v *visitor) error {
	encoder := func(msg *message) (string, error) {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(&msg); err != nil {
			return "", err
		}
		if msg.Event != messageEvent {
			return fmt.Sprintf("event: %s\ndata: %s\n", msg.Event, buf.String()), nil // Browser's .onmessage() does not fire on this!
		}
		return fmt.Sprintf("data: %s\n", buf.String()), nil
	}
	return s.handleSubscribe(w, r, v, "sse", "text/event-stream", encoder)
}

func (s *Server) handleSubscribeRaw(w http.ResponseWriter, r *http.Request, v *visitor) error {
	encoder := func(msg *message) (string, error) {
		if msg.Event == messageEvent { // only handle default events
			return strings.ReplaceAll(msg.Message, "\n", " ") + "\n", nil
		}
		return "\n", nil // "keepalive" and "open" events just send an empty line
	}
	return s.handleSubscribe(w, r, v, "raw", "text/plain", encoder)
}

func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request, v *visitor, format string, contentType string, encoder messageEncoder) error {
	if err := v.SubscriptionAllowed(); err != nil {
		return errHTTPTooManyRequestsLimitSubscriptions
	}
	defer v.RemoveSubscription()
	topicsStr := strings.TrimSuffix(r.URL.Path[1:], "/"+format) // Hack
	topicIDs := util.SplitNoEmpty(topicsStr, ",")
	topics, err := s.topicsFromIDs(topicIDs...)
	if err != nil {
		return err
	}
	poll := readParam(r, "x-poll", "poll", "po") == "1"
	scheduled := readParam(r, "x-scheduled", "scheduled", "sched") == "1"
	since, err := parseSince(r, poll)
	if err != nil {
		return err
	}
	messageFilter, titleFilter, priorityFilter, tagsFilter, err := parseQueryFilters(r)
	if err != nil {
		return err
	}
	var wlock sync.Mutex
	sub := func(msg *message) error {
		if !passesQueryFilter(msg, messageFilter, titleFilter, priorityFilter, tagsFilter) {
			return nil
		}
		m, err := encoder(msg)
		if err != nil {
			return err
		}
		wlock.Lock()
		defer wlock.Unlock()
		if _, err := w.Write([]byte(m)); err != nil {
			return err
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		return nil
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")            // CORS, allow cross-origin requests
	w.Header().Set("Content-Type", contentType+"; charset=utf-8") // Android/Volley client needs charset!
	if poll {
		return s.sendOldMessages(topics, since, scheduled, sub)
	}
	subscriberIDs := make([]int, 0)
	for _, t := range topics {
		subscriberIDs = append(subscriberIDs, t.Subscribe(sub))
	}
	defer func() {
		for i, subscriberID := range subscriberIDs {
			topics[i].Unsubscribe(subscriberID) // Order!
		}
	}()
	if err := sub(newOpenMessage(topicsStr)); err != nil { // Send out open message
		return err
	}
	if err := s.sendOldMessages(topics, since, scheduled, sub); err != nil {
		return err
	}
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-time.After(s.config.KeepaliveInterval):
			v.Keepalive()
			if err := sub(newKeepaliveMessage(topicsStr)); err != nil { // Send keepalive message
				return err
			}
		}
	}
}

func (s *Server) handleSubscribeWS(w http.ResponseWriter, r *http.Request, v *visitor) error {
	if err := v.SubscriptionAllowed(); err != nil {
		return errHTTPTooManyRequestsLimitSubscriptions
	}
	defer v.RemoveSubscription()
	topicsStr := strings.TrimSuffix(r.URL.Path[1:], "/ws") // Hack
	topicIDs := util.SplitNoEmpty(topicsStr, ",")
	topics, err := s.topicsFromIDs(topicIDs...)
	if err != nil {
		return err
	}
	poll := readParam(r, "x-poll", "poll", "po") == "1"
	scheduled := readParam(r, "x-scheduled", "scheduled", "sched") == "1"
	since, err := parseSince(r, poll)
	if err != nil {
		return err
	}
	messageFilter, titleFilter, priorityFilter, tagsFilter, err := parseQueryFilters(r)
	if err != nil {
		return err
	}
	upgrader := &websocket.Upgrader{
		ReadBufferSize:  wsBufferSize,
		WriteBufferSize: wsBufferSize,
		CheckOrigin: func(r *http.Request) bool {
			return true // We're open for business!
		},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	g, ctx := errgroup.WithContext(context.Background())
	g.Go(func() error {
		pongWait := s.config.KeepaliveInterval + wsPongWait
		conn.SetReadLimit(wsReadLimit)
		if err := conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
			return err
		}
		conn.SetPongHandler(func(appData string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})
		for {
			_, _, err := conn.NextReader()
			if err != nil {
				return err
			}
		}
	})
	g.Go(func() error {
		ping := func() error {
			if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return err
			}
			return conn.WriteMessage(websocket.PingMessage, nil)
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(s.config.KeepaliveInterval):
				v.Keepalive()
				if err := ping(); err != nil {
					return err
				}
			}
		}
	})
	sub := func(msg *message) error {
		if !passesQueryFilter(msg, messageFilter, titleFilter, priorityFilter, tagsFilter) {
			return nil
		}
		if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
			return err
		}
		return conn.WriteJSON(msg)
	}
	w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
	if poll {
		return s.sendOldMessages(topics, since, scheduled, sub)
	}
	subscriberIDs := make([]int, 0)
	for _, t := range topics {
		subscriberIDs = append(subscriberIDs, t.Subscribe(sub))
	}
	defer func() {
		for i, subscriberID := range subscriberIDs {
			topics[i].Unsubscribe(subscriberID) // Order!
		}
	}()
	if err := sub(newOpenMessage(topicsStr)); err != nil { // Send out open message
		return err
	}
	if err := s.sendOldMessages(topics, since, scheduled, sub); err != nil {
		return err
	}
	return g.Wait()
}

func parseQueryFilters(r *http.Request) (messageFilter string, titleFilter string, priorityFilter []int, tagsFilter []string, err error) {
	messageFilter = readParam(r, "x-message", "message", "m")
	titleFilter = readParam(r, "x-title", "title", "t")
	tagsFilter = util.SplitNoEmpty(readParam(r, "x-tags", "tags", "tag", "ta"), ",")
	priorityFilter = make([]int, 0)
	for _, p := range util.SplitNoEmpty(readParam(r, "x-priority", "priority", "prio", "p"), ",") {
		priority, err := util.ParsePriority(p)
		if err != nil {
			return "", "", nil, nil, err
		}
		priorityFilter = append(priorityFilter, priority)
	}
	return
}

func passesQueryFilter(msg *message, messageFilter string, titleFilter string, priorityFilter []int, tagsFilter []string) bool {
	if msg.Event != messageEvent {
		return true // filters only apply to messages
	}
	if messageFilter != "" && msg.Message != messageFilter {
		return false
	}
	if titleFilter != "" && msg.Title != titleFilter {
		return false
	}
	messagePriority := msg.Priority
	if messagePriority == 0 {
		messagePriority = 3 // For query filters, default priority (3) is the same as "not set" (0)
	}
	if len(priorityFilter) > 0 && !util.InIntList(priorityFilter, messagePriority) {
		return false
	}
	if len(tagsFilter) > 0 && !util.InStringListAll(msg.Tags, tagsFilter) {
		return false
	}
	return true
}

func (s *Server) sendOldMessages(topics []*topic, since sinceTime, scheduled bool, sub subscriber) error {
	if since.IsNone() {
		return nil
	}
	for _, t := range topics {
		messages, err := s.cache.Messages(t.ID, since, scheduled)
		if err != nil {
			return err
		}
		for _, m := range messages {
			if err := sub(m); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseSince returns a timestamp identifying the time span from which cached messages should be received.
//
// Values in the "since=..." parameter can be either a unix timestamp or a duration (e.g. 12h), or
// "all" for all messages.
func parseSince(r *http.Request, poll bool) (sinceTime, error) {
	since := readParam(r, "x-since", "since", "si")
	if since == "" {
		if poll {
			return sinceAllMessages, nil
		}
		return sinceNoMessages, nil
	}
	if since == "all" {
		return sinceAllMessages, nil
	} else if s, err := strconv.ParseInt(since, 10, 64); err == nil {
		return sinceTime(time.Unix(s, 0)), nil
	} else if d, err := time.ParseDuration(since); err == nil {
		return sinceTime(time.Now().Add(-1 * d)), nil
	}
	return sinceNoMessages, errHTTPBadRequestSinceInvalid
}

func (s *Server) handleOptions(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
	w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST")
	return nil
}

func (s *Server) topicFromPath(path string) (*topic, error) {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return nil, errHTTPBadRequestTopicInvalid
	}
	topics, err := s.topicsFromIDs(parts[1])
	if err != nil {
		return nil, err
	}
	return topics[0], nil
}

func (s *Server) topicsFromIDs(ids ...string) ([]*topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	topics := make([]*topic, 0)
	for _, id := range ids {
		if util.InStringList(disallowedTopics, id) {
			return nil, errHTTPBadRequestTopicDisallowed
		}
		if _, ok := s.topics[id]; !ok {
			if len(s.topics) >= s.config.TotalTopicLimit {
				return nil, errHTTPTooManyRequestsLimitTotalTopics
			}
			s.topics[id] = newTopic(id)
		}
		topics = append(topics, s.topics[id])
	}
	return topics, nil
}

func (s *Server) updateStatsAndPrune() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Expire visitors from rate visitors map
	for ip, v := range s.visitors {
		if v.Stale() {
			delete(s.visitors, ip)
		}
	}

	// Delete expired attachments
	if s.fileCache != nil {
		ids, err := s.cache.AttachmentsExpired()
		if err == nil {
			if err := s.fileCache.Remove(ids...); err != nil {
				log.Printf("error while deleting attachments: %s", err.Error())
			}
		} else {
			log.Printf("error retrieving expired attachments: %s", err.Error())
		}
	}

	// Prune message cache
	olderThan := time.Now().Add(-1 * s.config.CacheDuration)
	if err := s.cache.Prune(olderThan); err != nil {
		log.Printf("error pruning cache: %s", err.Error())
	}

	// Prune old topics, remove subscriptions without subscribers
	var subscribers, messages int
	for _, t := range s.topics {
		subs := t.Subscribers()
		msgs, err := s.cache.MessageCount(t.ID)
		if err != nil {
			log.Printf("cannot get stats for topic %s: %s", t.ID, err.Error())
			continue
		}
		if msgs == 0 && subs == 0 {
			delete(s.topics, t.ID)
			continue
		}
		subscribers += subs
		messages += msgs
	}

	// Mail stats
	var mailSuccess, mailFailure int64
	if s.smtpBackend != nil {
		mailSuccess, mailFailure = s.smtpBackend.Counts()
	}

	// Print stats
	log.Printf("Stats: %d message(s) published, %d in cache, %d successful mails, %d failed, %d topic(s) active, %d subscriber(s), %d visitor(s)",
		s.messages, messages, mailSuccess, mailFailure, len(s.topics), subscribers, len(s.visitors))
}

func (s *Server) runSMTPServer() error {
	sub := func(m *message) error {
		url := fmt.Sprintf("%s/%s", s.config.BaseURL, m.Topic)
		req, err := http.NewRequest("PUT", url, strings.NewReader(m.Message))
		if err != nil {
			return err
		}
		if m.Title != "" {
			req.Header.Set("Title", m.Title)
		}
		rr := httptest.NewRecorder()
		s.handle(rr, req)
		if rr.Code != http.StatusOK {
			return errors.New("error: " + rr.Body.String())
		}
		return nil
	}
	s.smtpBackend = newMailBackend(s.config, sub)
	s.smtpServer = smtp.NewServer(s.smtpBackend)
	s.smtpServer.Addr = s.config.SMTPServerListen
	s.smtpServer.Domain = s.config.SMTPServerDomain
	s.smtpServer.ReadTimeout = 10 * time.Second
	s.smtpServer.WriteTimeout = 10 * time.Second
	s.smtpServer.MaxMessageBytes = 1024 * 1024 // Must be much larger than message size (headers, multipart, etc.)
	s.smtpServer.MaxRecipients = 1
	s.smtpServer.AllowInsecureAuth = true
	return s.smtpServer.ListenAndServe()
}

func (s *Server) runManager() {
	for {
		select {
		case <-time.After(s.config.ManagerInterval):
			s.updateStatsAndPrune()
		case <-s.closeChan:
			return
		}
	}
}

func (s *Server) runAtSender() {
	for {
		select {
		case <-time.After(s.config.AtSenderInterval):
			if err := s.sendDelayedMessages(); err != nil {
				log.Printf("error sending scheduled messages: %s", err.Error())
			}
		case <-s.closeChan:
			return
		}
	}
}

func (s *Server) runFirebaseKeepliver() {
	if s.firebase == nil {
		return
	}
	for {
		select {
		case <-time.After(s.config.FirebaseKeepaliveInterval):
			if err := s.firebase(newKeepaliveMessage(firebaseControlTopic)); err != nil {
				log.Printf("error sending Firebase keepalive message: %s", err.Error())
			}
		case <-s.closeChan:
			return
		}
	}
}

func (s *Server) sendDelayedMessages() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages, err := s.cache.MessagesDue()
	if err != nil {
		return err
	}
	for _, m := range messages {
		t, ok := s.topics[m.Topic] // If no subscribers, just mark message as published
		if ok {
			if err := t.Publish(m); err != nil {
				log.Printf("unable to publish message %s to topic %s: %v", m.ID, m.Topic, err.Error())
			}
		}
		if s.firebase != nil { // Firebase subscribers may not show up in topics map
			if err := s.firebase(m); err != nil {
				log.Printf("unable to publish to Firebase: %v", err.Error())
			}
		}
		if err := s.cache.MarkPublished(m); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) withRateLimit(w http.ResponseWriter, r *http.Request, handler func(w http.ResponseWriter, r *http.Request, v *visitor) error) error {
	v := s.visitor(r)
	if err := v.RequestAllowed(); err != nil {
		return errHTTPTooManyRequestsLimitRequests
	}
	return handler(w, r, v)
}

// visitor creates or retrieves a rate.Limiter for the given visitor.
// This function was taken from https://www.alexedwards.net/blog/how-to-rate-limit-http-requests (MIT).
func (s *Server) visitor(r *http.Request) *visitor {
	s.mu.Lock()
	defer s.mu.Unlock()
	remoteAddr := r.RemoteAddr
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		ip = remoteAddr // This should not happen in real life; only in tests.
	}
	if s.config.BehindProxy && r.Header.Get("X-Forwarded-For") != "" {
		ip = r.Header.Get("X-Forwarded-For")
	}
	v, exists := s.visitors[ip]
	if !exists {
		s.visitors[ip] = newVisitor(s.config, ip)
		return s.visitors[ip]
	}
	v.Keepalive()
	return v
}

func (s *Server) inc(counter *int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*counter++
}
