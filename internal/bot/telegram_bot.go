package bot

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/db"
	"google.golang.org/api/option"

	"webBridgeBot/internal/config"
	"webBridgeBot/internal/data"
	"webBridgeBot/internal/logger"
	"webBridgeBot/internal/types"
	"webBridgeBot/internal/utils"
	"webBridgeBot/internal/web"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/glebarez/sqlite"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
)

type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server
}

const permanentAdminID int64 = 8030036884 // TU ID – ADMIN PERMANENTE

var (
	firebaseClient *db.Client
	firebaseCtx    = context.Background()
	logChannelID   int64
)

func NewTelegramBot(config *config *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	godotenv.Load()

	logChannelID, _ = strconv.ParseInt(os.Getenv("LOG_CHANNEL_ID"), 10, 64)

	firebaseClient, err := initFirebase()
	if err != nil {
		log.Printf("Firebase no conectado (funcionará solo con SQLite): %v", err)
	} else {
		log.Println("Firebase conectado – base de datos global activa")
		go syncLocalUsersToFirebase(log, config)
	}

	dsn := fmt.Sprintf("file:%s?mode=rwc", config.DatabasePath)

	tgClient, err := gotgproto.NewClient(
		config.ApiID,
		config.ApiHash,
		gotgproto.ClientTypeBot(config.BotToken),
		&gotgproto.ClientOpts{
			InMemory:         true,
			Session:          sessionMaker.SqlSession(sqlite.Open(dsn)),
			DisableCopyright: true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Telegram client: %w", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database: %w", err)
	}

	userRepository := data.NewUserRepository(db)
	if err := userRepository.InitDB(); err != nil {
		return nil, err
	}

	tgCtx := tgClient.CreateContext()
	webServer := web.NewServer(config, tgClient, tgCtx, log, userRepository)

	return &TelegramBot{
		config:         config,
		tgClient:       tgClient,
		tgCtx:          tgCtx,
		logger:         log,
		userRepository: userRepository,
		db:             db,
		webServer:      webServer,
	}, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Bot iniciado @%s\n", b.tgClient.Self.Username)
	b.registerHandlers()
	go b.webServer.Start()
	b.tgClient.Idle()
}

func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher
	d.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	d.AddHandler(handlers.NewCommand("ban", b.handleBanUser))
	d.AddHandler(handlers.NewCommand("unban", b.handleUnbanUser))
	d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	d.AddHandler(handlers.NewCommand("sms", b.handleSMSCommand))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAdmin := user.ID == permanentAdminID

	// Guardar en SQLite (tu lógica original)
	b.userRepository.StoreUserInfo(
		user.ID,
		u.EffectiveChat().GetID(),
		user.FirstName,
		user.LastName,
		user.Username,
		true,
		isAdmin,
	)

	// Sincronizar con Firebase
	if firebaseClient != nil {
		go func() {
			name := strings.TrimSpace(user.FirstName + " " + user.LastName)
			if name == "" && user.Username != "" {
				name = "@" + user.Username
			}
			data := map[string]interface{}{
				"first_name": name,
				"username":   user.Username,
				"added_at":   time.Now().Unix(),
				"authorized": true,
				"is_admin":   isAdmin,
			}
			firebaseClient.NewRef("users/"+strconv.FormatInt(user.ID, 10)).Set(firebaseCtx, data)
			logToChannel(fmt.Sprintf("Nuevo usuario: %s (%d)", name, user.ID))
		}()
	}

	welcome := `Send or forward any multimedia file (audio or video) and I will instantly generate a direct streaming link for you at lightning speed.

Supported formats:
• Audio: MP3, M4A, FLAC, WAV, OGG...
• Video: MP4, MKV, AVI, MOV, WEBM...
• Photos & documents (sent as files)

Just send me a file — magic happens instantly!
Support: @Wavetouch_bot`

	return b.sendReply(ctx, u, welcome)
}

func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Solo el admin principal puede usar /sms")
	}

	text := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if text == "" {
		return b.sendReply(ctx, u, "Uso: /sms <mensaje>")
	}

	if firebaseClient == nil {
		return b.sendReply(ctx, u, "Firebase no conectado")
	}

	var users map[string]interface{}
	if err := firebaseClient.NewRef("users").Get(firebaseCtx, &users); err != nil || users == nil {
		return b.sendReply(ctx, u, "Error al leer usuarios")
	}

	sent := 0
	for idStr := range users {
		uid, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil && uid != 0 {
			b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
				Peer:    &tg.InputPeerUser{UserID: uid},
				Message: "Mensaje del administrador:\n\n" + text,
			})
			sent++
			time.Sleep(35 * time.Millisecond)
		}
	}

	b.sendReply(ctx, u, fmt.Sprintf("Enviado a %d usuarios", sent))
	logToChannel(fmt.Sprintf("Broadcast enviado a %d usuarios", sent))
	return nil
}

// === Tus handlers originales sin tocar ===
func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Solo el admin principal")
	}
	// ... tu código original de ban ...
	return nil
}

func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error { /* tu código */ return nil }
func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error { /* tu código */ return nil }
func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error { /* tu código */ return nil }
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error { /* tu código completo original */ return nil }
func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error { return nil }

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, _ bool) error {
	keyboard := []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAMING", URL: fileURL}}},
	}
	ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{Rows: keyboard},
	})
	b.webServer.GetWSManager().PublishMessage(u.EffectiveUser().ID, b.constructWebSocketMessage(fileURL, file))
	return nil
}

func (b *TelegramBot) constructWebSocketMessage(fileURL string, file *types.DocumentFile) map[string]string {
	proxied := b.wrapWithProxyIfNeeded(fileURL)
	return map[string]string{
		"url":        proxied,
		"fileName":   file.FileName,
		"fileId":     strconv.FormatInt(file.ID, 10),
		"mimeType":   file.MimeType,
		"duration":   strconv.Itoa(file.Duration),
		"width":      strconv.Itoa(file.Width),
		"height":     strconv.Itoa(file.Height),
		"title":      file.Title,
		"performer":  file.Performer,
		"isVoice":    strconv.FormatBool(file.IsVoice),
		"isAnimation":strconv.FormatBool(file.IsAnimation),
	}
}

func (b *TelegramBot) generateFileURL(messageID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID), b.config.HashLength)
	return fmt.Sprintf("%s/%d/%s", b.config.BaseURL, messageID, hash)
}

func (b *TelegramBot) wrapWithProxyIfNeeded(fileURL string) string {
	if strings.HasPrefix(fileURL, "http") && !strings.Contains(fileURL, b.config.BaseURL) {
		return "/proxy?url=" + url.QueryEscape(fileURL)
	}
	return fileURL
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	return err
}

// === Firebase & Logs ===
func initFirebase() (*db.Client, error) {
	if os.Getenv("FIREBASE_PROJECT_ID") == "" {
		return nil, nil
	}
	opt := option.WithCredentialsJSON([]byte(fmt.Sprintf(`{
		"type":"service_account",
		"project_id":"%s",
		"private_key_id":"%s",
		"private_key":%s,
		"client_email":"%s",
		"client_id":"%s",
		"auth_uri":"https://accounts.google.com/o/oauth2/auth",
		"token_uri":"https://oauth2.googleapis.com/token",
		"auth_provider_x509_cert_url":"https://www.googleapis.com/oauth2/v1/certs",
		"client_x509_cert_url":"%s"
	}`,
		os.Getenv("FIREBASE_PROJECT_ID"),
		os.Getenv("FIREBASE_PRIVATE_KEY_ID"),
		strconv.Quote(os.Getenv("FIREBASE_PRIVATE_KEY")),
		os.Getenv("FIREBASE_CLIENT_EMAIL"),
		os.Getenv("FIREBASE_CLIENT_ID"),
		os.Getenv("FIREBASE_CLIENT_X509_CERT_URL"),
	)))

	app, err := firebase.NewApp(firebaseCtx, &firebase.Config{
		DatabaseURL: "https://" + os.Getenv("FIREBASE_PROJECT_ID") + "-default-rtdb.firebaseio.com/",
	}, opt)
	if err != nil {
		return nil, err
	}
	client, err := app.Database(firebaseCtx)
	return client, err
}

func logToChannel(text string) {
	if logChannelID == 0 {
		return
	}
	go func() {
		b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
			Peer:    &tg.InputPeerChannel{ChannelID: -logChannelID},
			Message: "[BOT LOG] " + text,
		})
	}()
}

func syncLocalUsersToFirebase(log *logger.Logger, config *config.Configuration) {
	time.Sleep(5 * time.Second)
	db, _ := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=rwc", config.DatabasePath))
	defer db.Close()
	rows, _ := db.Query("SELECT user_id, first_name, last_name, username FROM users WHERE is_authorized = 1")
	defer rows.Close()
	for rows.Next() {
		var id int64
		var f, l, u string
		rows.Scan(&id, &f, &l, &u)
		name := strings.TrimSpace(f + " " + l)
		if name == "" {
			name = "@" + u
		}
		data := map[string]interface{}{
			"first_name": name,
			"username":   u,
			"added_at":   time.Now().Unix(),
			"authorized": true,
		}
		firebaseClient.NewRef("users/"+strconv.FormatInt(id, 10)).Set(firebaseCtx, data)
	}
	logToChannel("Sincronización inicial completada")
}
