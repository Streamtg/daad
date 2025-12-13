package bot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"webBridgeBot/internal/config"
	"webBridgeBot/internal/logger"
	"webBridgeBot/internal/types"
	"webBridgeBot/internal/utils"
	"webBridgeBot/internal/web"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
)

type TelegramBot struct {
	config    *config.Configuration
	tgClient  *gotgproto.Client
	tgCtx     *ext.Context
	logger    *logger.Logger
	db        *sql.DB
	webServer *web.Server
}

const permanentAdminID int64 = 8030036884

var (
	logChannelID int64
	botInstance  *TelegramBot
)

type UserInfo struct {
	UserID       int64  `json:"user_id"`
	ChatID       int64  `json:"chat_id"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	Username     string `json:"username"`
	IsAuthorized bool   `json:"is_authorized"`
	IsAdmin      bool   `json:"is_admin"`
	CreatedAt    string `json:"created_at"`
}

func NewTelegramBot(cfg *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	_ = godotenv.Load()

	var err error
	if cfg.LogChannelID != "" {
		logChannelID, _ = strconv.ParseInt(cfg.LogChannelID, 10, 64)
	}

	db, err := sql.Open("sqlite3", "./webbridgebot.db")
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite: %w", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			user_id INTEGER PRIMARY KEY,
			chat_id INTEGER NOT NULL,
			first_name TEXT,
			last_name TEXT,
			username TEXT,
			is_authorized INTEGER DEFAULT 1,
			is_admin INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create table: %w", err)
	}

	log.Println("Connected to local SQLite database")

	// ================== FIX DEFINITIVO AL PANIC ==================
	session := sessionMaker.NewSessionStorage(
		sessionMaker.SessionStorageConfig{
			Type: sessionMaker.Memory,
		},
	)

	client, err := gotgproto.NewClient(
		cfg.ApiID,
		cfg.ApiHash,
		gotgproto.ClientTypeBot(cfg.BotToken),
		session,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram client: %w", err)
	}
	// ============================================================

	ctx := client.CreateContext()
	webSrv := web.NewServer(cfg, client, ctx, log, nil)

	b := &TelegramBot{
		config:    cfg,
		tgClient:  client,
		tgCtx:     ctx,
		logger:    log,
		db:        db,
		webServer: webSrv,
	}

	botInstance = b
	return b, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Bot started @%s", b.tgClient.Self.Username)
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
	d.AddHandler(handlers.NewCommand("syncdb", b.handleSyncDB))
	d.AddHandler(handlers.NewMessage(filters.Message.Edited, b.handleEditedPinnedMessage))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
}

/* ==================== USER MANAGEMENT ==================== */

func (b *TelegramBot) storeUserInfo(userID, chatID int64, firstName, lastName, username string, authorized, isAdmin bool) error {
	_, err := b.db.Exec(`
		INSERT OR REPLACE INTO users
		(user_id, chat_id, first_name, last_name, username, is_authorized, is_admin)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, chatID, firstName, lastName, username, authorized, isAdmin,
	)
	if err == nil {
		logToChannel(fmt.Sprintf("User stored: %d (@%s)", userID, username))
	}
	return err
}

func (b *TelegramBot) getUserInfo(userID int64) (*UserInfo, error) {
	var u UserInfo
	err := b.db.QueryRow(`
		SELECT user_id, chat_id, first_name, last_name, username, is_authorized, is_admin, created_at
		FROM users WHERE user_id = ?`, userID).
		Scan(&u.UserID, &u.ChatID, &u.FirstName, &u.LastName, &u.Username, &u.IsAuthorized, &u.IsAdmin, &u.CreatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &u, err
}

func (b *TelegramBot) setAuthorized(userID int64, authorized bool) error {
	_, err := b.db.Exec("UPDATE users SET is_authorized = ? WHERE user_id = ?", authorized, userID)
	return err
}

func (b *TelegramBot) getAllUsers() ([]UserInfo, error) {
	rows, err := b.db.Query("SELECT user_id, chat_id, first_name, last_name, username, is_authorized, is_admin, created_at FROM users")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []UserInfo
	for rows.Next() {
		var u UserInfo
		if err := rows.Scan(&u.UserID, &u.ChatID, &u.FirstName, &u.LastName, &u.Username, &u.IsAuthorized, &u.IsAdmin, &u.CreatedAt); err == nil {
			users = append(users, u)
		}
	}
	return users, nil
}

func (b *TelegramBot) getUserCount() (int, error) {
	var count int
	err := b.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	return count, err
}

/* ==================== HANDLERS ==================== */

func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAdmin := user.ID == permanentAdminID
	_ = b.storeUserInfo(user.ID, u.EffectiveChat().GetID(), user.FirstName, user.LastName, user.Username, true, isAdmin)

	msg := `Send or forward any multimedia file and I will instantly generate a streaming link.`
	return b.sendReply(ctx, u, msg)
}

func (b *TelegramBot) handleAnyUpdate(*ext.Context, *ext.Update) error {
	return nil
}

/* ==================== HELPERS ==================== */

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, text string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(text), nil)
	return err
}

func logToChannel(text string) {
	if botInstance == nil || logChannelID == 0 {
		return
	}
	go botInstance.tgClient.API().MessagesSendMessage(
		botInstance.tgCtx,
		&tg.MessagesSendMessageRequest{
			Peer:    &tg.InputPeerChannel{ChannelID: -logChannelID},
			Message: "[BOT LOG] " + text,
		},
	)
}
