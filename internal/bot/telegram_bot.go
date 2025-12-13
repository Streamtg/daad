package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"webBridgeBot/internal/config"
	"webBridgeBot/internal/logger"
	"webBridgeBot/internal/types"
	"webBridgeBot/internal/utils"
	"webBridgeBot/internal/web"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
)

type TelegramBot struct {
	config    *config.Configuration
	tgClient  *gotgproto.Client
	tgCtx     *ext.Context
	logger    *logger.Logger
	redis     *redis.Client
	webServer *web.Server
}

const permanentAdminID int64 = 8030036884

var (
	logChannelID int64
	botInstance  *TelegramBot
	ctx          = context.Background()
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

	// === CONEXIÓN A REDIS ===
	rdb := redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", cfg.RedisHost, cfg.RedisPort),
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})

	// Test conexión
	if err = rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}
	log.Println("Connected to Redis successfully")

	// Cliente Telegram
	client, err := gotgproto.NewClient(
		cfg.ApiID,
		cfg.ApiHash,
		gotgproto.ClientTypeBot(cfg.BotToken),
		&gotgproto.ClientOpts{
			InMemory:         true,
			DisableCopyright: true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram client: %w", err)
	}

	ctxTG := client.CreateContext()
	webSrv := web.NewServer(cfg, client, ctxTG, log, nil) // userRepo ya no es necesario en web si usas Redis directamente

	b := &TelegramBot{
		config:    cfg,
		tgClient:  client,
		tgCtx:     ctxTG,
		logger:    log,
		redis:     rdb,
		webServer: webSrv,
	}

	botInstance = b
	return b, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Bot started @%s\n", b.tgClient.Self.Username)
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
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
}

// ==================== USER MANAGEMENT CON REDIS ====================

func (b *TelegramBot) storeUserInfo(userID, chatID int64, firstName, lastName, username string, authorized, isAdmin bool) error {
	u := UserInfo{
		UserID:       userID,
		ChatID:       chatID,
		FirstName:    firstName,
		LastName:     lastName,
		Username:     username,
		IsAuthorized: authorized,
		IsAdmin:      isAdmin,
		CreatedAt:    time.Now().Format(time.RFC3339),
	}

	data, _ := json.Marshal(u)
	key := fmt.Sprintf("user:%d", userID)
	return b.redis.Set(ctx, key, data, 0).Err()
}

func (b *TelegramBot) getUserInfo(userID int64) (*UserInfo, error) {
	key := fmt.Sprintf("user:%d", userID)
	data, err := b.redis.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var u UserInfo
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (b *TelegramBot) setAuthorized(userID int64, authorized bool) error {
	u, err := b.getUserInfo(userID)
	if err != nil || u == nil {
		return fmt.Errorf("user not found")
	}
	u.IsAuthorized = authorized
	data, _ := json.Marshal(u)
	return b.redis.Set(ctx, fmt.Sprintf("user:%d", userID), data, 0).Err()
}

func (b *TelegramBot) getAllUsers() ([]UserInfo, error) {
	keys, err := b.redis.Keys(ctx, "user:*").Result()
	if err != nil {
		return nil, err
	}

	var users []UserInfo
	for _, key := range keys {
		data, err := b.redis.Get(ctx, key).Bytes()
		if err != nil {
			continue
		}
		var u UserInfo
		if json.Unmarshal(data, &u) == nil {
			users = append(users, u)
		}
	}
	return users, nil
}

func (b *TelegramBot) getUserCount() (int, error) {
	return len(b.redis.Keys(ctx, "user:*").Val()), nil
}

// ==================== HANDLERS ====================

func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAdmin := user.ID == permanentAdminID

	_ = b.storeUserInfo(
		user.ID,
		u.EffectiveChat().GetID(),
		user.FirstName,
		user.LastName,
		user.Username,
		true,
		isAdmin,
	)

	logToChannel(fmt.Sprintf("New user: %s %s (@%s) - ID: %d", user.FirstName, user.LastName, user.Username, user.ID))

	welcome := `Send or forward any multimedia file... (tu mensaje de bienvenida aquí)`

	return b.sendReply(ctx, u, welcome)
}

func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only admin")
	}

	message := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if message == "" {
		return b.sendReply(ctx, u, "Usage: /sms <message>")
	}

	users, err := b.getAllUsers()
	if err != nil {
		return b.sendReply(ctx, u, "Error loading users")
	}

	sent := 0
	for _, usr := range users {
		if usr.IsAuthorized && usr.UserID != permanentAdminID && usr.ChatID != 0 {
			_, err := b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
				Peer:    &tg.InputPeerUser{UserID: usr.UserID},
				Message: "Message from admin:\n\n" + message,
			})
			if err == nil {
				sent++
			}
			time.Sleep(33 * time.Millisecond)
		}
	}

	b.sendReply(ctx, u, fmt.Sprintf("Message sent to %d users.", sent))
	logToChannel(fmt.Sprintf("Broadcast sent to %d users", sent))
	return nil
}

// Los demás handlers (ban, unban, listusers, userinfo, media) se adaptan igual usando b.getUserInfo, b.setAuthorized, etc.

func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only admin")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /ban <user_id> [reason]")
	}
	targetID, _ := strconv.ParseInt(args[1], 10, 64)
	_ = b.setAuthorized(targetID, false)
	reason := "No reason"
	if len(args) > 2 {
		reason = strings.Join(args[2:], " ")
	}
	b.sendReply(ctx, u, fmt.Sprintf("User %d banned. Reason: %s", targetID, reason))
	return nil
}

// ... (el resto de handlers similares)

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, text string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{})
	return err
}

func logToChannel(text string) {
	if logChannelID == 0 || botInstance == nil {
		return
	}
	go botInstance.tgClient.API().MessagesSendMessage(botInstance.tgCtx, &tg.MessagesSendMessageRequest{
		Peer:    &tg.InputPeerChannel{ChannelID: -logChannelID},
		Message: "[BOT LOG] " + text,
	})
}
