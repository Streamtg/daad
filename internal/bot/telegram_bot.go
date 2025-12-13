package bot

import (
	"context"
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

	if cfg.LogChannelID != "" {
		logChannelID, _ = strconv.ParseInt(cfg.LogChannelID, 10, 64)
	}

	db, err := sql.Open("sqlite3", "./webbridgebot.db")
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS users (
		user_id INTEGER PRIMARY KEY,
		chat_id INTEGER,
		first_name TEXT,
		last_name TEXT,
		username TEXT,
		is_authorized INTEGER DEFAULT 1,
		is_admin INTEGER DEFAULT 0,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`)
	if err != nil {
		return nil, err
	}

	// ================= FIX SESSION BETA21 =================
	session, closeSession, err := sessionMaker.NewSessionStorage(
		context.Background(),
		sessionMaker.InMemorySession,
		true,
	)
	if err != nil {
		return nil, err
	}
	_ = closeSession
	// =====================================================

	client, err := gotgproto.NewClient(
		cfg.ApiID,
		cfg.ApiHash,
		gotgproto.ClientTypeBot(cfg.BotToken),
		session,
	)
	if err != nil {
		return nil, err
	}

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
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
}

/* ================= HANDLERS ================= */

func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	isAdmin := user.ID == permanentAdminID
	b.storeUserInfo(user.ID, u.EffectiveChat().GetID(), user.FirstName, user.LastName, user.Username, true, isAdmin)

	return b.sendReply(ctx, u, "Send a media file and I will generate a streaming link.")
}

func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	return nil
}
func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error {
	return nil
}
func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	return nil
}
func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	return nil
}
func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	return nil
}
func (b *TelegramBot) handleSyncDB(ctx *ext.Context, u *ext.Update) error {
	return nil
}

func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	return nil
}

func (b *TelegramBot) handleAnyUpdate(*ext.Context, *ext.Update) error {
	return nil
}

/* ================= HELPERS ================= */

func (b *TelegramBot) storeUserInfo(userID, chatID int64, fn, ln, un string, auth, admin bool) {
	b.db.Exec(
		`INSERT OR REPLACE INTO users VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		userID, chatID, fn, ln, un, auth, admin,
	)
}

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
			Message: text,
		},
	)
}
