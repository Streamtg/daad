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

type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server
}

func NewTelegramBot(config *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	dsn := fmt.Sprintf("file:%s?mode=rwc", config.DatabasePath)

	tgClient, err := gotgproto.NewClient(
		config.ApiID,
		config.ApiHash,
		gotgproto.ClientTypeBot(config.BotToken),
		&gotgproto.ClientOpts{
			InMemory:          true,
			Session:           sessionMaker.SqlSession(sqlite.Open(dsn)),
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

func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s)...\n", b.tgClient.Self.Username)
	b.registerHandlers()
	go b.webServer.Start()
	if err := b.tgClient.Idle(); err != nil {
		b.logger.Fatalf("Failed to start Telegram client: %s", err)
	}
}

func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher

	d.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	d.AddHandler(handlers.NewCommand("ban", b.handleBanUser))
	d.AddHandler(handlers.NewCommand("unban", b.handleUnbanUser))
	d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))

	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

// /start – Auto-authorize + English welcome
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isFirstUser, _ := b.userRepository.IsFirstUser()
	isAdmin := isFirstUser
	isAuthorized := true

	err := b.userRepository.StoreUserInfo(
		user.ID,
		u.EffectiveChat().GetID(),
		user.FirstName,
		user.LastName,
		user.Username,
		isAuthorized,
		isAdmin,
	)
	if err != nil {
		b.logger.Printf("Failed to store user %d: %v", user.ID, err)
		return err
	}

	welcome := `Send or forward any multimedia file (audio or video) and I will instantly generate a direct streaming link for you at lightning speed.

Supported formats:
• Audio: MP3, M4A, FLAC, WAV, OGG...
• Video: MP4, MKV, AVI, MOV, WEBM...
• Photos & documents (sent as files)

How to use me:
• Personal media host (movies, series, documentaries)
• Share large videos without compression
• Build your private streaming library
• Stream directly in browser from any device
• Access your files anywhere, anytime

Just send me a file — magic happens instantly!`

	return b.sendReply(ctx, u, welcome)
}

// /ban – Admin only
func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	adminID := u.EffectiveUser().ID
	adminInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !adminInfo.IsAdmin {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /ban <user_id> [reason]")
	}

	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || targetID <= 0 {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}
	if targetID == adminID {
		return b.sendReply(ctx, u, "You cannot ban yourself.")
	}

	reason := "No reason provided"
	if len(args) > 2 {
		reason = strings.Join(args[2:], " ")
	}

	err = b.userRepository.DeauthorizeUser(targetID)
	if err != nil {
		return b.sendReply(ctx, u, "Failed to ban user.")
	}

	b.logger.Printf("ADMIN %d banned user %d – Reason: %s", adminID, targetID, reason)

	go func() {
		info, _ := b.userRepository.GetUserInfo(targetID)
		if info != nil && info.ChatID != 0 {
			peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
			b.tgCtx.SendMessage(info.ChatID, &tg.MessagesSendMessageRequest{
				Peer:    peer,
				Message: fmt.Sprintf("You have been permanently banned from using this bot.\n\nReason: %s\n\nIf you believe this is a mistake, contact the administrator.", reason),
			})
		}
	}()

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been banned.\nReason: %s", targetID, reason))
}

// /unban – Admin only
func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error {
	adminID := u.EffectiveUser().ID
	adminInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !adminInfo.IsAdmin {
		return b.sendReply(ctx, u, "Only the administrator can use this command.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /unban <user_id>")
	}

	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || targetID <= 0 {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	err = b.userRepository.AuthorizeUser(targetID, false)
	if err != nil {
		return b.sendReply(ctx, u, "Failed to unban user.")
	}

	b.logger.Printf("ADMIN %d unbanned user %d", adminID, targetID)

	go func() {
		info, _ := b.userRepository.GetUserInfo(targetID)
		if info != nil && info.ChatID != 0 {
			peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
			b.tgCtx.SendMessage(info.ChatID, &tg.MessagesSendMessageRequest{
				Peer:    peer,
				Message: "You have been unbanned!\nYou can now use the bot again.",
			})
		}
	}()

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been unbanned.", targetID))
}

// Media handling
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	userID := user.ID

	userInfo, err := b.userRepository.GetUserInfo(userID)
	if err != nil || !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot.")
	}

	b.logger.Printf("Media received from authorized user %d (%s)", userID, user.FirstName)

	if b.config.LogChannelID != "" && b.config.LogChannelID != "0" {
		go b.forwardToLogChannel(ctx, u)
	}

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if webPage, ok := u.EffectiveMessage.Message.Media.(*tg.MessageMediaWebPage); ok {
			if _, empty := webPage.Webpage.(*tg.WebPageEmpty); empty {
				if link := utils.ExtractURLFromEntities(u.EffectiveMessage.Message); link != "" {
					mime := utils.DetectMimeTypeFromURL(link)
					file = &types.DocumentFile{
						FileName: "external_link",
						MimeType: mime,
						FileSize: 0,
					}
					return b.sendMediaToUser(ctx, u, link, file, false)
				}
			}
		}
		return b.sendReply(ctx, u, "Unsupported file or link.")
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	return b.sendMediaToUser(ctx, u, fileURL, file, false)
}

// Admin: /listusers
func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	adminID := u.EffectiveUser().ID
	info, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !info.IsAdmin {
		return b.sendReply(ctx, u, "Only the administrator can use this command.")
	}

	const pageSize = 10
	page := 1
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) > 1 {
		if p, err := strconv.Atoi(args[1]); err == nil && p > 0 {
			page = p
		}
	}

	total, _ := b.userRepository.GetUserCount()
	offset := (page - 1) * pageSize
	users, _ := b.userRepository.GetAllUsers(offset, pageSize)

	if len(users) == 0 {
		return b.sendReply(ctx, u, "No users found or empty page.")
	}

	var msg strings.Builder
	msg.WriteString("*User List*\n\n")
	for i, usr := range users {
		status := "Authorized"
		if !usr.IsAuthorized {
			status = "Banned"
		}
		adminTag := ""
		if usr.IsAdmin {
			adminTag = " (Admin)"
		}
		uname := usr.Username
		if uname == "" {
			uname = "N/A"
		}
		msg.WriteString(fmt.Sprintf("%d. ID: `%d` – %s %s (@%s) – %s%s\n",
			offset+i+1, usr.UserID, usr.FirstName, usr.LastName, uname, status, adminTag))
	}
	totalPages := (total + pageSize - 1) / pageSize
	msg.WriteString(fmt.Sprintf("\nPage %d of %d (%d total users)", page, totalPages, total))

	return b.sendReply(ctx, u, msg.String())
}

// Admin: /userinfo
func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	adminID := u.EffectiveUser().ID
	info, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !info.IsAdmin {
		return b.sendReply(ctx, u, "Only the administrator can use this command.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /userinfo <user_id>")
	}

	targetID, _ := strconv.ParseInt(args[1], 10, 64)
	target, err := b.userRepository.GetUserInfo(targetID)
	if err != nil {
		return b.sendReply(ctx, u, "User not found.")
	}

	status := "Authorized"
	if !target.IsAuthorized {
		status = "Banned"
	}
	adminStatus := "No"
	if target.IsAdmin {
		adminStatus = "Yes"
	}
	uname := target.Username
	if uname == "" {
		uname = "N/A"
	}

	msg := fmt.Sprintf(`*User Info*
ID: %d
Chat ID: %d
Name: %s %s
Username: @%s
Status: %s
Admin: %s
Joined: %s`,
		target.UserID, target.ChatID, target.FirstName, target.LastName,
		uname, status, adminStatus, target.CreatedAt.Format("02 Jan 2006 15:04"))

	return b.sendReply(ctx, u, msg)
}

// Helper functions (unchanged)
func (b *TelegramBot) forwardToLogChannel(ctx *ext.Context, u *ext.Update) {
	// Keep your existing log channel code here
}

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, _ bool) error {
	keyboard := []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAMING", URL: fileURL}}},
	}

	_, err := ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{Rows: keyboard},
	})
	if err != nil {
		b.logger.Printf("Failed to send media reply: %v", err)
		return err
	}

	wsMsg := b.constructWebSocketMessage(fileURL, file)
	b.webServer.GetWSManager().PublishMessage(u.EffectiveUser().ID, wsMsg)
	return nil
}

func (b *TelegramBot) constructWebSocketMessage(fileURL string, file *types.DocumentFile) map[string]string {
	proxied := b.wrapWithProxyIfNeeded(fileURL)
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

func (b *TelegramBot) generateFileURL(messageID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID), b.config.HashLength)
	return fmt.Sprintf("%s/%d/%s", b.config.BaseURL, messageID, hash)
}

func (b *TelegramBot) wrapWithProxyIfNeeded(fileURL string) string {
	if strings.HasPrefix(fileURL, "http://") || strings.HasPrefix(fileURL, "https://") {
		if !strings.Contains(fileURL, ":"+b.config.Port) &&
			!strings.Contains(fileURL, "localhost") &&
			!strings.HasPrefix(fileURL, b.config.BaseURL) {
			return "/proxy?url=" + url.QueryEscape(fileURL)
		}
	}
	return fileURL
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error {
	if b.config.DebugMode {
		// Your debug logs
	}
	return nil
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{ParseMode: "Markdown"})
	if err != nil {
		b.logger.Printf("Reply error: %v", err)
	}
	return err
}
