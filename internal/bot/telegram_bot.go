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
	"firebaseDB "firebase.google.com/go/v4/db"
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

const permanentAdminID int64 = 8030036884 // Your permanent admin ID

var (
	firebaseClient *firebaseDB.Client
	firebaseCtx    = context.Background()
	logChannelID   int64
)

func NewTelegramBot(cfg *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	godotenv.Load()

	logChannelID, _ = strconv.ParseInt(os.Getenv("LOG_CHANNEL_ID"), 10, 64)

	// Initialize Firebase (optional – bot works without it using only SQLite)
	var err error
	firebaseClient, err = initFirebase()
	if err != nil {
		log.Printf("Firebase not connected (running with local SQLite only): %v", err)
	} else {
		log.Println("Firebase connected – global user database active")
		go syncLocalUsersToFirebase(log, cfg)
	}

	dsn := fmt.Sprintf("file:%s?mode=rwc", cfg.DatabasePath)

	client, err := gotgproto.NewClient(
		cfg.ApiID,
		cfg.ApiHash,
		gotgproto.ClientTypeBot(cfg.BotToken),
		&gotgproto.ClientOpts{
			InMemory:         true,
			Session:          sessionMaker.SqlSession(sqlite.Open(dsn)),
			DisableCopyright: true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram client: %w", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database: %w", err)
	}

	userRepo := data.NewUserRepository(db)
	if err := userRepo.InitDB(); err != nil {
		return nil, err
	}

	ctx := client.CreateContext()
	webSrv := web.NewServer(cfg, client, ctx, log, userRepo)

	return &TelegramBot{
		config:         cfg,
		tgClient:       client,
		tgCtx:          ctx,
		logger:         log,
		userRepository: userRepo,
		db:             db,
		webServer:      webSrv,
	}, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Bot started @%s\n", b.tgClient.Self.Username)
	b.registerHandlers()
	go b.webServer.Start()
	b.tgClient.Idle()
}

func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher
	d.AddHandler(handlers.NewCommand("start", b.handleStart))
	d.AddHandler(handlers.NewCommand("ban", b.handleBan))
	d.AddHandler(handlers.NewCommand("unban", b.handleUnban))
	d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	d.AddHandler(handlers.NewCommand("sms", b.handleSMS)) // New broadcast command
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMedia))
}

// /start – auto-authorize every user
func (b *TelegramBot) handleStart(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAdmin := user.ID == permanentAdminID

	// Save locally
	b.userRepository.StoreUserInfo(user.ID, u.EffectiveChat().GetID(), user.FirstName, user.LastName, user.Username, true, isAdmin)

	// Sync to Firebase if available
	if firebaseClient != nil {
		go func() {
			name := strings.TrimSpace(user.FirstName + " " + user.LastName)
			if name == "" && user.Username != "" {
				name = "@" + user.Username
			}
			data := map[string]interface{}{
				"name":       name,
				"username":   user.Username,
				"added_at":   time.Now().Unix(),
				"authorized": true,
				"is_admin":   isAdmin,
			}
			firebaseClient.NewRef("users/"+strconv.FormatInt(user.ID, 10)).Set(firebaseCtx, data)
			logToChannel(fmt.Sprintf("New user: %s (%d)", name, user.ID))
		}()
	}

	welcome := `Send or forward any audio/video file and I'll instantly generate a direct streaming link.

Supported: MP3, M4A, MP4, MKV, WEBM, documents as files, etc.

Just send a file — link appears in seconds!
Support: @Wavetouch_bot`

	return b.reply(ctx, u, welcome)
}

// /sms <message> – broadcast only for permanent admin
func (b *TelegramBot) handleSMS(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.reply(ctx, u, "Only the main admin can use /sms")
	}

	text := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if text == "" {
		return b.reply(ctx, u, "Usage: /sms <your message>")
	}

	if firebaseClient == nil {
		return b.reply(ctx, u, "Firebase not connected – broadcast unavailable")
	}

	var users map[string]interface{}
	if err := firebaseClient.NewRef("users").Get(firebaseCtx, &users); err != nil || users == nil {
		return b.reply(ctx, u, "Failed to read user list")
	}

	sent := 0
	for idStr := range users {
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil && id != 0 {
			b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
				Peer:    &tg.InputPeerUser{UserID: id},
				Message: "Admin message:\n\n" + text,
			})
			sent++
			time.Sleep(35 * time.Millisecond)
		}
	}

	b.reply(ctx, u, fmt.Sprintf("Message sent to %d users", sent))
	logToChannel(fmt.Sprintf("Broadcast sent to %d users", sent))
	return nil
}

// Keep your original handlers unchanged
func (b *TelegramBot) handleBan(ctx *ext.Context, u *ext.Update) error       { /* your code */ return nil }
func (b *TelegramBot) handleUnban(ctx *ext.Context, u *ext.Update) error    { /* your code */ return nil }
func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error { /* your code */ return nil }
func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error  { /* your code */ return nil }
func (b *TelegramBot) handleMedia(ctx *ext.Context, u *ext.Update) error     { /* your original media code */ return nil }
func (b *TelegramBot) handleAnyUpdate(*ext.Context, *ext.Update) error      { return nil }

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, _ bool) error {
	keyboard := []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAM", URL: fileURL}}},
	}
	ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{Rows: keyboard},
	})
	b.webServer.GetWSManager().PublishMessage(u.EffectiveUser().ID, b.buildWSMessage(fileURL, file))
	return nil
}

func (b *TelegramBot) buildWSMessage(fileURL string, file *types.DocumentFile) map[string]string {
	proxied := b.proxyIfNeeded(fileURL)
	return map[string]string{
		"url":         proxied,
		"fileName":    file.FileName,
		"fileId":      strconv.FormatInt(file.ID, 10),
		"mimeType":    file.MimeType,
		"duration":    strconv.Itoa(file.Duration),
		"width":       strconv.Itoa(file.Width),
		"height":      strconv.Itoa(file.Height),
		"title":       file.Title,
		"performer":   file.Performer,
		"isVoice":     strconv.FormatBool(file.IsVoice),
		"isAnimation": strconv.FormatBool(file.IsAnimation),
	}
}

func (b *TelegramBot) generateFileURL(msgID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID), b.config.HashLength)
	return fmt.Sprintf("%s/%d/%s", b.config.BaseURL, msgID, hash)
}

func (b *TelegramBot) proxyIfNeeded(url string) string {
	if strings.HasPrefix(url, "http") && !strings.Contains(url, b.config.BaseURL) {
		return "/proxy?url=" + url.QueryEscape(url)
	}
	return url
}

func (b *TelegramBot) reply(ctx *ext.Context, u *ext.Update, text string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{})
	return err
}

// Firebase initialization
func initFirebase() (*firebaseDB.Client, error) {
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

func syncLocalUsersToFirebase(log *logger.Logger, cfg *config.Configuration) {
	time.Sleep(5 * time.Second)
	db, _ := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=rwc", cfg.DatabasePath))
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
			"name":       name,
			"username":   u,
			"added_at":   time.Now().Unix(),
			"authorized": true,
		}
		firebaseClient.NewRef("users/"+strconv.FormatInt(id, 10)).Set(firebaseCtx, data)
	}
	logToChannel("Initial sync completed")
}
