package bot

import (
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"time"
	"webBridgeBot/internal/config"
	"webBridgeBot/internal/data"
	"webBridgeBot/internal/logger"
	"webBridgeBot/internal/types"
	"webBridgeBot/internal/utils"
	"webBridgeBot/internal/web"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/celestix/gotgproto/storage"
	"github.com/glebarez/sqlite"
	"github.com/gotd/td/tg"
)

const (
	callbackResendToPlayer = "cb_ResendToPlayer"
	callbackPlay           = "cb_Play"
	callbackRestart        = "cb_Restart"
	callbackForward10      = "cb_Fwd10"
	callbackBackward10     = "cb_Bwd10"
	callbackToggleFullscreen = "cb_ToggleFullscreen"
	callbackListUsers      = "cb_listusers"
	callbackUserAuthAction = "cb_user_auth_action"
)

// TelegramBot representa la estructura principal del bot.
type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server
}

// NewTelegramBot crea una nueva instancia de TelegramBot.
func NewTelegramBot(config *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	dsn := fmt.Sprintf("file:%s?mode=rwc", config.DatabasePath)
	tgClient, err := gotgproto.NewClient(
		config.ApiID,
		config.ApiHash,
		gotgproto.ClientTypeBot(config.BotToken),
		&gotgproto.ClientOpts{
			InMemory:         true,
			Session:          sessionMaker.SqlSession(sqlite.Open(dsn)),
			DisableCopyright: true,
		})
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

// Run inicia el bot de Telegram y el servidor web.
func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s)...\n", b.tgClient.Self.Username)
	b.registerHandlers()
	go b.webServer.Start()
	if err := b.tgClient.Idle(); err != nil {
		b.logger.Fatalf("Failed to start Telegram client: %s", err)
	}
}

func (b *TelegramBot) registerHandlers() {
	clientDispatcher := b.tgClient.Dispatcher
	clientDispatcher.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	clientDispatcher.AddHandler(handlers.NewCommand("authorize", b.handleAuthorizeUser))
	clientDispatcher.AddHandler(handlers.NewCommand("deauthorize", b.handleDeauthorizeUser))
	clientDispatcher.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	clientDispatcher.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	clientDispatcher.AddHandler(handlers.NewCallbackQuery(filters.CallbackQuery.Prefix("cb_"), b.handleCallbackQuery))
	clientDispatcher.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	clientDispatcher.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

// --- HANDLERS DE COMANDOS Y CALLBACKS ---

func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	existingUser, err := b.userRepository.GetUserInfo(user.ID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	isFirstUser, _ := b.userRepository.IsFirstUser()
	isAdmin := false
	isAuthorized := false

	if existingUser == nil {
		if isFirstUser {
			isAdmin = true
			isAuthorized = true
		}
		b.userRepository.StoreUserInfo(user.ID, chatID, user.FirstName, user.LastName, user.Username, isAuthorized, isAdmin)
		if !isAdmin {
			go b.notifyAdminsAboutNewUser(user, chatID)
		}
	} else {
		isAuthorized = existingUser.IsAuthorized
		isAdmin = existingUser.IsAdmin
	}

	webURL := fmt.Sprintf("%s/%d", b.config.BaseURL, chatID)
	startMsg := fmt.Sprintf("Hello %s, I am @%s!\nOpen your player: %s", user.FirstName, ctx.Self.Username, webURL)
	_ = b.sendMediaURLReply(ctx, u, startMsg, webURL)

	if !isAuthorized {
		return b.sendReply(ctx, u, "You are not authorized yet. Please ask an admin.")
	}
	return nil
}

func (b *TelegramBot) notifyAdminsAboutNewUser(newUser *tg.User, newUsersChatID int64) {
	admins, _ := b.userRepository.GetAllAdmins()
	var notificationMsg string
	username, hasUsername := newUser.GetUsername()
	if hasUsername {
		notificationMsg = fmt.Sprintf("New user: @%s", username)
	} else {
		notificationMsg = fmt.Sprintf("New user: %s %s", newUser.FirstName, newUser.LastName)
	}
	markup := &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{Text: "✅ Authorize", Data: []byte(fmt.Sprintf("%s,%d,authorize", callbackUserAuthAction, newUser.ID))},
					&tg.KeyboardButtonCallback{Text: "❌ Decline", Data: []byte(fmt.Sprintf("%s,%d,decline", callbackUserAuthAction, newUser.ID))},
				},
			},
		},
	}

	for _, admin := range admins {
		if admin.UserID == newUser.ID && admin.UserID == newUsersChatID {
			continue
		}
		peer := b.tgCtx.PeerStorage.GetInputPeerById(admin.ChatID)
		req := &tg.MessagesSendMessageRequest{Peer: peer, Message: notificationMsg, ReplyMarkup: markup}
		_, _ = b.tgCtx.SendMessage(admin.ChatID, req)
	}
}

// --- AUTORIZACIÓN / DESAUTORIZACIÓN ---

func (b *TelegramBot) handleAuthorizeUser(ctx *ext.Context, u *ext.Update) error {
	adminID := u.EffectiveUser().ID
	userInfo, _ := b.userRepository.GetUserInfo(adminID)
	if !userInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /authorize <user_id> [admin]")
	}
	targetUserID, _ := strconv.ParseInt(args[1], 10, 64)
	isAdmin := len(args) > 2 && args[2] == "admin"
	_ = b.userRepository.AuthorizeUser(targetUserID, isAdmin)
	return b.sendReply(ctx, u, fmt.Sprintf("User %d authorized", targetUserID))
}

func (b *TelegramBot) handleDeauthorizeUser(ctx *ext.Context, u *ext.Update) error {
	adminID := u.EffectiveUser().ID
	userInfo, _ := b.userRepository.GetUserInfo(adminID)
	if !userInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /deauthorize <user_id>")
	}
	targetUserID, _ := strconv.ParseInt(args[1], 10, 64)
	_ = b.userRepository.DeauthorizeUser(targetUserID)
	return b.sendReply(ctx, u, fmt.Sprintf("User %d deauthorized", targetUserID))
}

// --- MENSAJES DE MEDIOS ---

func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	existingUser, err := b.userRepository.GetUserInfo(chatID)
	if err != nil || !existingUser.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to send media.")
	}

	// Se puede expandir para forward, videos y documentos
	if u.EffectiveMessage.Document != nil || u.EffectiveMessage.Video != nil {
		fileID := ""
		if u.EffectiveMessage.Video != nil {
			fileID = u.EffectiveMessage.Video.GetFileID()
		} else if u.EffectiveMessage.Document != nil {
			fileID = u.EffectiveMessage.Document.GetFileID()
		}
		fileURL := b.generateFileURL(fileID, chatID)
		b.webServer.SendToPlayer(chatID, fileURL)
		return b.sendReply(ctx, u, fmt.Sprintf("Media received. Streaming at: %s", fileURL))
	}
	return nil
}

// --- FUNCIONES AUXILIARES ---

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	peer := u.EffectiveChat().InputPeer()
	_, err := ctx.SendMessage(peer, &tg.MessagesSendMessageRequest{Peer: peer, Message: msg})
	return err
}

func (b *TelegramBot) sendMediaURLReply(ctx *ext.Context, u *ext.Update, msg, url string) error {
	peer := u.EffectiveChat().InputPeer()
	markup := &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonURL{Text: "Open Player", URL: url},
			}},
		},
	}
	_, err := ctx.SendMessage(peer, &tg.MessagesSendMessageRequest{
		Peer:        peer,
		Message:     msg,
		ReplyMarkup: markup,
	})
	return err
}

// Genera URL única para streaming del archivo
func (b *TelegramBot) generateFileURL(fileID string, chatID int64) string {
	randPart := rand.Intn(999999)
	return fmt.Sprintf("%s/stream/%d/%s?rand=%d", b.config.BaseURL, chatID, url.PathEscape(fileID), randPart)
}

// --- CALLBACKS ---

func (b *TelegramBot) handleCallbackQuery(ctx *ext.Context, cq *tg.CallbackQuery) error {
	// Aquí se implementa la lógica de todos los callbacks: Play, Pause, Forward, Fullscreen, Resend...
	return nil
}

func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	users, _ := b.userRepository.GetAllUsers()
	lines := []string{"Users list:"}
	for _, user := range users {
		lines = append(lines, fmt.Sprintf("- %s (%d) Authorized: %v Admin: %v", user.Username, user.UserID, user.IsAuthorized, user.IsAdmin))
	}
	return b.sendReply(ctx, u, strings.Join(lines, "\n"))
}

func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /userinfo <user_id>")
	}
	userID, _ := strconv.ParseInt(args[1], 10, 64)
	info, _ := b.userRepository.GetUserInfo(userID)
	if info == nil {
		return b.sendReply(ctx, u, "User not found")
	}
	return b.sendReply(ctx, u, fmt.Sprintf("User %s (%d)\nAuthorized: %v\nAdmin: %v", info.Username, info.UserID, info.IsAuthorized, info.IsAdmin))
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error {
	// Placeholder para cualquier update adicional
	return nil
}
