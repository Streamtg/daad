package bot

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
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

const permanentAdminID int64 = 8030036884

var (
	firebaseClient *db.Client
	firebaseCtx    = context.Background()
	logChannelID   int64
	botInstance    *TelegramBot
)

func NewTelegramBot(cfg *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	godotenv.Load()

	var err error
	logChannelID, _ = strconv.ParseInt(os.Getenv("LOG_CHANNEL_ID"), 10, 64)

	firebaseClient, err = initFirebase()
	if err != nil {
		log.Printf("Firebase not connected (local mode): %v", err)
	} else {
		log.Println("Firebase connected — global sync active")
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
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open db: %w", err)
	}

	userRepo := data.NewUserRepository(db)
	if err := userRepo.InitDB(); err != nil {
		return nil, err
	}

	ctx := client.CreateContext()
	webSrv := web.NewServer(cfg, client, ctx, log, userRepo)

	b := &TelegramBot{
		config:         cfg,
		tgClient:       client,
		tgCtx:          ctx,
		logger:         log,
		userRepository: userRepo,
		db:             db,
		webServer:      webSrv,
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
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

// ==================== MEDIA + REENVÍO AL CANAL (CORREGIDO Y FUNCIONANDO) ====================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(userID)
	if err != nil || !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot.")
	}

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if webPage, ok := u.EffectiveMessage.Message.Media.(*tg.MessageMediaWebPage); ok {
			if _, empty := webPage.Webpage.(*tg.WebPageEmpty); empty {
				if link := utils.ExtractURLFromEntities(u.EffectiveMessage.Message); link != "" {
					mime := utils.DetectMimeTypeFromURL(link)
					file = &types.DocumentFile{FileName: "external_link", MimeType: mime}
					return b.sendMediaToUser(ctx, u, link, file, false)
				}
			}
		}
		return b.sendReply(ctx, u, "Unsupported media.")
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)

	// REENVÍO AL CANAL DE LOGS (CORREGIDO: ya no pasa string, pasa el ID correcto)
	if logChannelID != 0 {
		go func() {
			fromChatID := u.EffectiveChat().GetID()
			messageID := u.EffectiveMessage.Message.ID

			forwardReq := &tg.MessagesForwardMessagesRequest{
				FromPeer: &tg.InputPeerChat{ChatID: fromChatID},
				ID:       []int{messageID},
				RandomID: []int64{rand.Int63()},
				ToPeer:   &tg.InputPeerChannel{ChannelID: logChannelID}, // ← CORRECTO: ID numérico puro
			}

			result, err := ctx.Raw.MessagesForwardMessages(ctx, forwardReq)
			if err != nil {
				b.logger.Printf("Failed to forward to log channel: %v", err)
				return
			}

			var forwardedMsgID int
			for _, upd := range result.Updates {
				if newMsg, ok := upd.(*tg.UpdateNewChannelMessage); ok {
					if m, ok := newMsg.Message.(*tg.Message); ok {
						forwardedMsgID = m.ID
						break
					}
				}
			}

			if forwardedMsgID == 0 {
				return
			}

			userName := strings.TrimSpace(userInfo.FirstName + " " + userInfo.LastName)
			if userName == "" && userInfo.Username != "" {
				userName = "@" + userInfo.Username
			}
			if userName == "" {
				userName = "User"
			}

			infoMsg := fmt.Sprintf(`File uploaded
User: %s (%d)
File: %s
Link: %s`,
				userName, userID, file.FileName, fileURL)

			_, err = ctx.Raw.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     &tg.InputPeerChannel{ChannelID: logChannelID},
				Message:  infoMsg,
				ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: forwardedMsgID},
				RandomID: rand.Int63(),
			})
			if err != nil {
				b.logger.Printf("Failed to send info to log channel: %v", err)
			}
		}()
	}

	return b.sendMediaToUser(ctx, u, fileURL, file, false)
}

// ==================== RESTO DEL CÓDIGO (100% COMPLETO Y FUNCIONAL) ====================
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAdmin := user.ID == permanentAdminID

	if err := b.userRepository.StoreUserInfo(
		user.ID,
		u.EffectiveChat().GetID(),
		user.FirstName,
		user.LastName,
		user.Username,
		true,
		isAdmin,
	); err != nil {
		b.logger.Printf("Failed to store user %d: %v", user.ID, err)
	}

	if firebaseClient != nil {
		go func() {
			name := strings.TrimSpace(user.FirstName + " " + user.LastName)
			if name == "" && user.Username != "" {
				name = "@" + user.Username
			}
			firebaseClient.NewRef("users/"+strconv.FormatInt(user.ID, 10)).Set(firebaseCtx, map[string]interface{}{
				"display_name": name,
				"username":     user.Username,
				"added_at":     time.Now().Unix(),
				"authorized":   true,
				"is_admin":     isAdmin,
			})
		}()
	}

	return b.sendReply(ctx, u, `Send or forward any multimedia file (audio or video) and I will instantly generate a direct streaming link for you at lightning speed.

Supported formats:
• Audio: MP3, M4A, FLAC, WAV, OGG...
• Video: MP4, MKV, AVI, MOV, WEBM...
• Photos & documents (sent as files)

Just send me a file — magic happens instantly!
Support: @Wavetouch_bot`)
}

func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}

	msg := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if msg == "" {
		return b.sendReply(ctx, u, "Usage: /sms <your message>")
	}

	if firebaseClient == nil {
		return b.sendReply(ctx, u, "Firebase not available")
	}

	var users map[string]interface{}
	if err := firebaseClient.NewRef("users").Get(firebaseCtx, &users); err != nil || users == nil {
		return b.sendReply(ctx, u, "Failed to load users")
	}

	sent := 0
	for idStr := range users {
		if uid, err := strconv.ParseInt(idStr, 10, 64); err == nil && uid != 0 {
			b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
				Peer:    &tg.InputPeerUser{UserID: uid},
				Message: "Admin message:\n\n" + msg,
			})
			sent++
			time.Sleep(33 * time.Millisecond)
		}
	}

	b.sendReply(ctx, u, fmt.Sprintf("Message sent to %d users", sent))
	return nil
}

func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
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
	if targetID == permanentAdminID {
		return b.sendReply(ctx, u, "You cannot ban the main administrator.")
	}
	reason := "No reason provided"
	if len(args) > 2 {
		reason = strings.Join(args[2:], " ")
	}
	if err := b.userRepository.DeauthorizeUser(targetID); err != nil {
		return b.sendReply(ctx, u, "Failed to ban user.")
	}
	b.logger.Printf("ADMIN %d banned user %d - Reason: %s", permanentAdminID, targetID, reason)
	go func() {
		info, _ := b.userRepository.GetUserInfo(targetID)
		if info != nil && info.ChatID != 0 {
			peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
			b.tgCtx.SendMessage(info.ChatID, &tg.MessagesSendMessageRequest{
				Peer:    peer,
				Message: fmt.Sprintf("You have been permanently banned from using this bot.\nSupport: @Wavetouch_bot\n\nReason: %s", reason),
			})
		}
	}()
	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been banned.\nReason: %s", targetID, reason))
}

func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
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
	if err := b.userRepository.AuthorizeUser(targetID, false); err != nil {
		return b.sendReply(ctx, u, "Failed to unban user.")
	}
	b.logger.Printf("ADMIN %d unbanned user %d", permanentAdminID, targetID)
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

func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
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
	if total == 0 {
		return b.sendReply(ctx, u, "No users registered yet.")
	}
	offset := (page - 1) * pageSize
	users, _ := b.userRepository.GetAllUsers(offset, pageSize)
	if len(users) == 0 {
		return b.sendReply(ctx, u, "No users on this page.")
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
		username := usr.Username
		if username == "" {
			username = "N/A"
		}
		msg.WriteString(fmt.Sprintf("%d. `%d` - %s %s (@%s) - %s%s\n",
			offset+i+1, usr.UserID, usr.FirstName, usr.LastName, username, status, adminTag))
	}
	totalPages := (total + pageSize - 1) / pageSize
	msg.WriteString(fmt.Sprintf("\nPage %d of %d (%d total users)", page, totalPages, total))
	return b.sendReply(ctx, u, msg.String())
}

func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the administrator can use this command.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /userinfo <user_id>")
	}
	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}
	target, err := b.userRepository.GetUserInfo(targetID)
	if err != nil || target == nil {
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
	username := target.Username
	if username == "" {
		username = "N/A"
	}
	msg := fmt.Sprintf(`*User Information*
ID: <code>%d</code>
Name: %s %s
Username: @%s
Status: %s
Admin: %s
Joined: %s`,
		target.UserID, target.FirstName, target.LastName, username, status, adminStatus, target.CreatedAt)
	return b.sendReply(ctx, u, msg)
}

func (b *TelegramBot) handleAnyUpdate(*ext.Context, *ext.Update) error { return nil }

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
	if strings.HasPrefix(fileURL, "http") && !strings.Contains(fileURL, b.config.BaseURL) {
		return "/proxy?url=" + url.QueryEscape(fileURL)
	}
	return fileURL
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Reply error: %v", err)
	}
	return err
}

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

func syncLocalUsersToFirebase(log *logger.Logger, cfg *config.Configuration) {
	time.Sleep(5 * time.Second)
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=rwc", cfg.DatabasePath))
	if err != nil {
		return
	}
	defer db.Close()

	rows, err := db.Query("SELECT user_id, first_name, last_name, username FROM users WHERE is_authorized = 1")
	if err != nil {
		log.Printf("Sync error: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var first, last, username string
		rows.Scan(&id, &first, &last, &username)
		name := strings.TrimSpace(first + " " + last)
		if name == "" {
			name = "@" + username
		}
		data := map[string]interface{}{
			"display_name": name,
			"username":     username,
			"added_at":     time.Now().Unix(),
			"authorized":   true,
		}
		firebaseClient.NewRef("users/"+strconv.FormatInt(id, 10)).Set(firebaseCtx, data)
	}
}
