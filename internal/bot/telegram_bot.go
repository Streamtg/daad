package bot

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"time"

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
	"github.com/glebarez/sqlite"
	"github.com/gotd/td/tg"
)

const (
	permanentAdminID     int64 = 8030036884 // TU ID – ADMIN ÚNICO Y PERMANENTE
	logChannelMarker          = "DB_USER:"
)

type UserInfo struct {
	UserID       int64  `json:"id"`
	ChatID       int64  `json:"chat_id"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	Username     string `json:"username"`
	IsAuthorized bool   `json:"authorized"`
	IsAdmin      bool   `json:"admin"`
	CreatedAt    string `json:"created_at"`
}

type TelegramBot struct {
	config    *config.Configuration
	tgClient  *gotgproto.Client
	tgCtx     *ext.Context
	logger    *logger.Logger
	webServer *web.Server

	// Cache en memoria para no leer el canal cada vez
	userCache map[int64]*UserInfo
}

func NewTelegramBot(config *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	tgClient, err := gotgproto.NewClient(
		config.ApiID,
		config.ApiHash,
		gotgproto.ClientTypeBot(config.BotToken),
		&gotgproto.ClientOpts{
			InMemory:         true,
			Session:          sessionMaker.SqlSession(sqlite.Open("session.db")),
			DisableCopyright: true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Telegram client: %w", err)
	}

	tgCtx := tgClient.CreateContext()
	webServer := web.NewServer(config, tgClient, tgCtx, log, nil) // sin repo local

	bot := &TelegramBot{
		config:    config,
		tgClient:  tgClient,
		tgCtx:     tgCtx,
		logger:    log,
		webServer: webServer,
		userCache: make(map[int64]*UserInfo),
	}

	// Cargar base de datos desde el canal al iniciar
	if config.LogChannelID != "" {
		go bot.loadUsersFromLogChannel()
	}

	return bot, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Bot iniciado (@%s) | DB en canal: %s", b.tgClient.Self.Username, b.config.LogChannelID)
	b.registerHandlers()
	go b.webServer.Start()
	if err := b.tgClient.Idle(); err != nil {
		b.logger.Fatalf("Error fatal: %v", err)
	}
}

func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher
	d.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	d.AddHandler(handlers.NewCommand("sms", b.handleSMSCommand))     // NUEVO
	d.AddHandler(handlers.NewCommand("ban", b.handleBanUser))
	d.AddHandler(handlers.NewCommand("unban", b.handleUnbanUser))
	d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
}

// ==================== /sms – ENVÍA MENSAJE A TODOS ====================
func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Solo el admin principal puede usar /sms")
	}

	text := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if text == "" || text == "/sms" {
		return b.sendReply(ctx, u, "Uso: /sms <mensaje a enviar a todos>")
	}

	users := b.getAllUsersFromCache()
	success := 0
	for _, user := range users {
		if !user.IsAuthorized {
			continue
		}
		_, err := b.tgCtx.SendMessage(user.ChatID, &tg.MessagesSendMessageRequest{
			Peer:     b.tgCtx.PeerStorage.GetInputPeerById(user.ChatID),
			Message:  fmt.Sprintf("Mensaje del admin:\n\n%s", text),
			RandomID: rand.Int63(),
		})
		if err == nil {
			success++
		}
		time.Sleep(50 * time.Millisecond) // Anti-flood
	}

	return b.sendReply(ctx, u, fmt.Sprintf("Mensaje enviado a %d usuarios", success))
}

// ==================== CARGA USUARIOS DESDE CANAL DE LOGS ====================
func (b *TelegramBot) loadUsersFromLogChannel() {
	b.logger.Printf("Cargando base de datos desde canal %s...", b.config.LogChannelID)
	peer, err := utils.GetLogChannelPeer(b.tgCtx, b.config.LogChannelID)
	if err != nil {
		b.logger.Printf("Error al obtener peer del canal: %v", err)
		return
	}

	var users []UserInfo
	offsetID := 0
	limit := 100

	for {
		messages, err := b.tgCtx.GetHistory(peer, offsetID, limit)
		if err != nil {
			b.logger.Printf("Error leyendo canal: %v", err)
			break
		}
		if len(messages.Messages) == 0 {
			break
		}

		for _, msg := range messages.Messages {
			m, ok := msg.(*tg.Message)
			if !ok || m.Message == "" {
				continue
			}
			if !strings.HasPrefix(m.Message, logChannelMarker) {
				continue
			}

			jsonData := strings.TrimPrefix(m.Message, logChannelMarker)
			var user UserInfo
			if json.Unmarshal([]byte(jsonData), &user) == nil {
				users = append(users, user)
				if m.ID > offsetID {
					offsetID = m.ID
				}
			}
		}
		if len(messages.Messages) < limit {
			break
		}
	}

	for _, u := range users {
		b.userCache[u.UserID] = &u
	}
	b.logger.Printf("Base de datos cargada: %d usuarios", len(users))
}

// ==================== GUARDAR USUARIO EN CANAL ====================
func (b *TelegramBot) saveUserToLogChannel(user *UserInfo) error {
	if b.config.LogChannelID == "" {
		return nil
	}

	data, _ := json.Marshal(user)
	message := logChannelMarker + string(data)

	peer, err := utils.GetLogChannelPeer(b.tgCtx, b.config.LogChannelID)
	if err != nil {
		return err
	}

	_, err = b.tgCtx.SendMessage(peer, &tg.MessagesSendMessageRequest{
		Message:  message,
		RandomID: rand.Int63(),
	})
	return err
}

// ==================== OBTENER USUARIOS ====================
func (b *TelegramBot) getUserInfo(userID int64) *UserInfo {
	if u, ok := b.userCache[userID]; ok {
		return u
	}
	return nil
}

func (b *TelegramBot) getAllUsersFromCache() []*UserInfo {
	var list []*UserInfo
	for _, u := range b.userCache {
		list = append(list, u)
	}
	return list
}

// ==================== /start ====================
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	existing := b.getUserInfo(user.ID)
	isNew := existing == nil

	if isNew {
		newUser := &UserInfo{
			UserID:       user.ID,
			ChatID:       u.EffectiveChat().GetID(),
			FirstName:    user.FirstName,
			LastName:     user.LastName,
			Username:     user.Username,
			IsAuthorized: user.ID == permanentAdminID, // Tú eres autorizado automáticamente
			IsAdmin:      user.ID == permanentAdminID,
			CreatedAt:    time.Now().Format(time.RFC3339),
		}
		b.userCache[user.ID] = newUser
		b.saveUserToLogChannel(newUser)
		b.logger.Printf("Nuevo usuario registrado: %d (@%s)", user.ID, user.Username)
	}

	welcome := `Envía o reenvía cualquier archivo multimedia y te doy link de streaming instantáneo.

Formatos soportados:
• Video: MP4, MKV AVI MOV WEBM
• Audio: MP3 FLAC WAV OGG OPUS
• Fotos y documentos

Funciona en iPhone, Android, PC, Smart TV
Sin compresión · Seek perfecto · 4K/8K

Solo envía un archivo y listo

Soporte: @Wavetouch_bot`

	return b.sendReply(ctx, u, welcome)
}

// ==================== Media Handler (con streaming MKV) ====================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID
	userInfo := b.getUserInfo(userID)

	if userInfo == nil {
		return b.sendReply(ctx, u, "Primero usa /start")
	}
	if !userInfo.IsAuthorized && userID != permanentAdminID {
		return b.sendReply(ctx, u, "No estás autorizado. Contacta al admin.")
	}

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if link := utils.ExtractURLFromEntities(u.EffectiveMessage.Message); link != "" {
			file = &types.DocumentFile{FileName: "video.mkv", MimeType: "video/mp4"}
			return b.sendMediaToUser(ctx, u, link, file, false)
		}
		return b.sendReply(ctx, u, "Archivo no soportado.")
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	return b.sendMediaToUser(ctx, u, fileURL, file, false)
}

func (b *TelegramBot) generateFileURL(messageID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(fmt.Sprintf("%d%s%d", messageID, file.FileName, file.FileSize), 8)
	return fmt.Sprintf("%s/%d/%s", strings.TrimRight(b.config.BaseURL, "/"), messageID, hash)
}

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, _ bool) error {
	keyboard := []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAM NOW", URL: fileURL}}},
	}

	_, err := ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{Rows: keyboard},
	})
	if err != nil {
		b.logger.Printf("Error reply: %v", err)
	}

	wsMsg := map[string]string{
		"url":      b.wrapWithProxyIfNeeded(fileURL),
		"fileName": file.FileName,
		"mimeType": "video/mp4", // TRUCO MKV
	}
	b.webServer.GetWSManager().PublishMessage(u.EffectiveUser().ID, wsMsg)
	return nil
}

func (b *TelegramBot) wrapWithProxyIfNeeded(fileURL string) string {
	if strings.HasPrefix(fileURL, "http") &&
		!strings.Contains(fileURL, b.config.BaseURL) {
		return "/proxy?url=" + url.QueryEscape(fileURL)
	}
	return fileURL
}

// ==================== Admin Commands ====================
func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Solo el admin principal puede usar este comando.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "/ban <user_id>")
	}
	id, _ := strconv.ParseInt(args[1], 10, 64)
	if user := b.getUserInfo(id); user != nil {
		user.IsAuthorized = false
		b.saveUserToLogChannel(user)
	}
	return b.sendReply(ctx, u, fmt.Sprintf("Usuario %d baneado.", id))
}

func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Solo el admin principal.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "/unban <user_id>")
	}
	id, _ := strconv.ParseInt(args[1], 10, 64)
	if user := b.getUserInfo(id); user != nil {
		user.IsAuthorized = true
		b.saveUserToLogChannel(user)
	}
	return b.sendReply(ctx, u, fmt.Sprintf("Usuario %d desbaneado.", id))
}

func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return nil
	}
	users := b.getAllUsersFromCache()
	msg := "*Lista de usuarios:*\n\n"
	for i, user := range users {
		status := "Baneado"
		if user.IsAuthorized {
			status = "Autorizado"
		}
		msg += fmt.Sprintf("%d. `%d` – %s (@%s) – %s\n", i+1, user.UserID, user.FirstName, user.Username, status)
	}
	return b.sendReply(ctx, u, msg)
}

func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return nil
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "/userinfo <id>")
	}
	id, _ := strconv.ParseInt(args[1], 10, 64)
	user := b.getUserInfo(id)
	if user == nil {
		return b.sendReply(ctx, u, "Usuario no encontrado.")
	}
	return b.sendReply(ctx, u, fmt.Sprintf("ID: %d\nNombre: %s\nUser: @%s\nEstado: %t", user.UserID, user.FirstName, user.Username, user.IsAuthorized))
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Error reply: %v", err)
	}
	return err
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error { return nil }
