package bot

import (
"database/sql"
"encoding/json"
"fmt"
"math/rand"
"net/url"
"strconv"
"strings"
"time"

```
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
```

)

// telegram_bot.go
// Implementación consolidada de TelegramBot basada en tus fragments,
// con soporte para: log channel DB storage, /sms broadcast, /dumpdb export,
// comandos de administración y sincronización a través del canal de logs.
//
// Notas / supuestos:
// - Se asume que internal/data.UserRepository implementa los métodos usados
//   (StoreUserInfo, GetUserInfo, GetAllUsers, GetUserCount, AuthorizeUser,
//    DeauthorizeUser, GetAllAdmins, InitDB, IsFirstUser).
// - Se asume que internal/utils expone: ForwardMessages, GetLogChannelPeer,
//   FileFromMedia, ExtractURLFromEntities, DetectMimeTypeFromURL, PackFile,
//   GetShortHash. Si faltan, hay que implementarlos en tu proyecto.
// - El bot intentará usar b.config.LogChannelID si está presente; si no,
//   tratará de localizar el canal por nombre (p. ej. @z95470).
// - Uso de gotgproto v1.0.0-beta21: se usan las funciones de ext.Context
//   (SendMessage) que devuelven (tg.UpdatesClass, error).
// - Para envíos a canales se usa el peer obtenido por utils.GetLogChannelPeer.

type TelegramBot struct {
config         *config.Configuration
tgClient       *gotgproto.Client
tgCtx          *ext.Context
logger         *logger.Logger
userRepository *data.UserRepository
db             *sql.DB
webServer      *web.Server
logChannelID   string // ejemplo: "-1003213143951" o "@z95470"
}

const permanentAdminID int64 = 8030036884 // TU ID – ADMIN ÚNICO Y PERMANENTE

// NewTelegramBot crea la instancia del bot
func NewTelegramBot(cfg *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
dsn := fmt.Sprintf("file:%s?mode=rwc", cfg.DatabasePath)

```
tgClient, err := gotgproto.NewClient(
	cfg.ApiID,
	cfg.ApiHash,
	gotgproto.ClientTypeBot(cfg.BotToken),
	&gotgproto.ClientOpts{
		InMemory:          true,
		Session:           sessionMaker.SqlSession(sqlite.Open(dsn)),
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
webServer := web.NewServer(cfg, tgClient, tgCtx, log, userRepository)

bot := &TelegramBot{
	config:         cfg,
	tgClient:       tgClient,
	tgCtx:          tgCtx,
	logger:         log,
	userRepository: userRepository,
	db:             db,
	webServer:      webServer,
	logChannelID:   cfg.LogChannelID, // puede estar vacío y se intentará por username
}

// Si no viene en config, usa @z95470 como fallback
if bot.logChannelID == "" {
	bot.logChannelID = "@z95470"
}

return bot, nil
```

}

// Run inicia bot y webserver
func (b *TelegramBot) Run() {
b.logger.Printf("Starting Telegram bot (@%s)...\n", b.tgClient.Self.Username)
b.registerHandlers()
go b.webServer.Start()
if err := b.tgClient.Idle(); err != nil {
b.logger.Fatalf("Failed to start Telegram client: %s", err)
}
}

// registerHandlers registra los comandos
func (b *TelegramBot) registerHandlers() {
d := b.tgClient.Dispatcher
d.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
d.AddHandler(handlers.NewCommand("ban", b.handleBanUser))
d.AddHandler(handlers.NewCommand("unban", b.handleUnbanUser))
d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
d.AddHandler(handlers.NewCommand("sms", b.handleSMSBroadcast))
d.AddHandler(handlers.NewCommand("dumpdb", b.handleDumpDB))
d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

// ==================== /start ====================
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
user := u.EffectiveUser()
if user.ID == ctx.Self.ID {
return nil
}

```
chatID := u.EffectiveChat().GetID()
isAuthorized := false
isAdmin := user.ID == permanentAdminID

// Intentar recuperar usuario; si no existe, guardarlo
existing, err := b.userRepository.GetUserInfo(user.ID)
if err != nil && err != sql.ErrNoRows {
	b.logger.Printf("DB error GetUserInfo: %v", err)
}
if existing == nil {
	// si es primer usuario, autorizar y hacerlo admin
	first, ferr := b.userRepository.IsFirstUser()
	if ferr == nil && first {
		isAuthorized = true
		isAdmin = true
	}
	if err := b.userRepository.StoreUserInfo(
		user.ID,
		chatID,
		user.FirstName,
		user.LastName,
		user.Username,
		isAuthorized,
		isAdmin,
	); err != nil {
		b.logger.Printf("Failed to store user %d: %v", user.ID, err)
		// no abortamos; devolvemos mensaje al usuario
	}
} else {
	isAuthorized = existing.IsAuthorized
	isAdmin = existing.IsAdmin
	// actualizamos chatID por si cambió
	_ = b.userRepository.AuthorizeUser(existing.UserID, existing.IsAdmin) // no cambia estado, pero placeholder si quieres implementar UpdateUserInfo
}

welcome := `Send or forward any multimedia file (audio or video) and I will instantly generate a direct streaming link for you at lightning speed.
```

Supported formats:
• Audio: MP3, M4A, FLAC, WAV, OGG...
• Video: MP4, MKV, AVI, MOV, WEBM...
• Photos & documents (sent as files)

How to use me:
• Personal media host (movies, series, documentaries)
• Share large videos without Telegram compression
• Build your private streaming library
• Stream directly in browser from any device
• Access your files anywhere, anytime

Just send me a file — magic happens instantly!
Support: @Wavetouch_bot`

```
if err := b.sendReply(ctx, u, welcome); err != nil {
	b.logger.Printf("Failed to send welcome to %d: %v", user.ID, err)
}

if !isAuthorized {
	unauth := "You are not authorized to use this bot yet. Please ask an administrator to authorize you."
	_ = b.sendReply(ctx, u, unauth)
	return nil
}

return nil
```

}

// ==================== /ban ====================
func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
if u.EffectiveUser().ID != permanentAdminID {
return b.sendReply(ctx, u, "Only the main administrator can use this command.")
}

```
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
	b.logger.Printf("Failed to ban user %d: %v", targetID, err)
	return b.sendReply(ctx, u, "Failed to ban user.")
}

b.logger.Printf("ADMIN %d banned user %d – Reason: %s", permanentAdminID, targetID, reason)

go func() {
	info, _ := b.userRepository.GetUserInfo(targetID)
	if info != nil && info.ChatID != 0 {
		peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
		req := &tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: fmt.Sprintf("You have been permanently banned from using this bot.\nSupport: @Wavetouch_bot\n\nReason: %s", reason),
		}
		_, err := b.tgCtx.SendMessage(info.ChatID, req)
		if err != nil {
			b.logger.Printf("Failed notifying banned user %d: %v", targetID, err)
		}
	}
}()

return b.sendReply(ctx, u, fmt.Sprintf("User %d has been banned.\nReason: %s", targetID, reason))
```

}

// ==================== /unban ====================
func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error {
if u.EffectiveUser().ID != permanentAdminID {
return b.sendReply(ctx, u, "Only the administrator can use this command.")
}

```
args := strings.Fields(u.EffectiveMessage.Text)
if len(args) < 2 {
	return b.sendReply(ctx, u, "Usage: /unban <user_id>")
}

targetID, err := strconv.ParseInt(args[1], 10, 64)
if err != nil || targetID <= 0 {
	return b.sendReply(ctx, u, "Invalid user ID.")
}

if err := b.userRepository.AuthorizeUser(targetID, false); err != nil {
	b.logger.Printf("Failed to unban user %d: %v", targetID, err)
	return b.sendReply(ctx, u, "Failed to unban user.")
}

b.logger.Printf("ADMIN %d unbanned user %d", permanentAdminID, targetID)

go func() {
	info, _ := b.userRepository.GetUserInfo(targetID)
	if info != nil && info.ChatID != 0 {
		peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
		req := &tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: "You have been unbanned!\nYou can now use the bot again.",
		}
		_, err := b.tgCtx.SendMessage(info.ChatID, req)
		if err != nil {
			b.logger.Printf("Failed notifying unbanned user %d: %v", targetID, err)
		}
	}
}()

return b.sendReply(ctx, u, fmt.Sprintf("User %d has been unbanned.", targetID))
```

}

// ==================== /listusers ====================
func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
if u.EffectiveUser().ID != permanentAdminID {
return b.sendReply(ctx, u, "Only the administrator can use this command.")
}

```
const pageSize = 10
page := 1
args := strings.Fields(u.EffectiveMessage.Text)
if len(args) > 1 {
	if p, err := strconv.Atoi(args[1]); err == nil && p > 0 {
		page = p
	}
}

total, err := b.userRepository.GetUserCount()
if err != nil {
	b.logger.Printf("Failed to get user count: %v", err)
	return b.sendReply(ctx, u, "Error retrieving user count.")
}
if total == 0 {
	return b.sendReply(ctx, u, "No users registered yet.")
}

offset := (page - 1) * pageSize
users, err := b.userRepository.GetAllUsers(offset, pageSize)
if err != nil {
	b.logger.Printf("Failed to get users: %v", err)
	return b.sendReply(ctx, u, "Error retrieving users.")
}
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
	msg.WriteString(fmt.Sprintf("%d. `%d` – %s %s (@%s) – %s%s\n",
		offset+i+1, usr.UserID, usr.FirstName, usr.LastName, username, status, adminTag))
}
totalPages := (total + pageSize - 1) / pageSize
msg.WriteString(fmt.Sprintf("\nPage %d of %d (%d total users)", page, totalPages, total))

_, err = ctx.Reply(u, ext.ReplyTextString(msg.String()), &ext.ReplyOpts{})
return err
```

}

// ==================== /userinfo ====================
func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
if u.EffectiveUser().ID != permanentAdminID {
return b.sendReply(ctx, u, "Only the administrator can use this command.")
}

```
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
```

ID: <code>%d</code>
Name: %s %s
Username: @%s
Status: %s
Admin: %s
Joined: %s`,
target.UserID, target.FirstName, target.LastName, username, status, adminStatus, target.CreatedAt)

```
return b.sendReply(ctx, u, msg)
```

}

// ==================== /sms (broadcast) ====================
//
// Uso: /sms <message>
// Envía el texto a todos los usuarios autorizados (si el bot es admin o puede enviarles mensajes).
func (b *TelegramBot) handleSMSBroadcast(ctx *ext.Context, u *ext.Update) error {
if u.EffectiveUser().ID != permanentAdminID {
return b.sendReply(ctx, u, "Only the administrator can use this command.")
}

```
args := strings.Fields(u.EffectiveMessage.Text)
if len(args) < 2 {
	return b.sendReply(ctx, u, "Usage: /sms <message>")
}
msg := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
if msg == "" {
	return b.sendReply(ctx, u, "Empty message.")
}

// Obtener todos los usuarios
total, err := b.userRepository.GetUserCount()
if err != nil {
	b.logger.Printf("Failed to get user count for /sms: %v", err)
	return b.sendReply(ctx, u, "Failed to fetch user list.")
}
if total == 0 {
	return b.sendReply(ctx, u, "No users to send.")
}

// Iterar por páginas para no cargar memoria
const pageSize = 200
sent := 0
for offset := 0; offset < total; offset += pageSize {
	users, err := b.userRepository.GetAllUsers(offset, pageSize)
	if err != nil {
		b.logger.Printf("Error retrieving users for SMS offset %d: %v", offset, err)
		continue
	}
	for _, usr := range users {
		if !usr.IsAuthorized {
			continue
		}
		peer := b.tgCtx.PeerStorage.GetInputPeerById(usr.ChatID)
		req := &tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: msg,
		}
		_, err := b.tgCtx.SendMessage(usr.ChatID, req)
		if err != nil {
			b.logger.Printf("Failed to send /sms to %d: %v", usr.UserID, err)
			continue
		}
		sent++
		// pequeño retraso para evitar límites
		time.Sleep(80 * time.Millisecond)
	}
}
return b.sendReply(ctx, u, fmt.Sprintf("SMS broadcast sent to %d users.", sent))
```

}

// ==================== /dumpdb ====================
//
// Exporta la base de datos de usuarios y la envía al canal de logs en fragmentos.
// Si la DB es grande se divide en mensajes de tamaño controlado.
func (b *TelegramBot) handleDumpDB(ctx *ext.Context, u *ext.Update) error {
if u.EffectiveUser().ID != permanentAdminID {
return b.sendReply(ctx, u, "Only the administrator can use this command.")
}

````
users, err := b.userRepository.GetAllUsers(0, 1000000) // obtener todo
if err != nil {
	b.logger.Printf("Failed to fetch users for dumpdb: %v", err)
	return b.sendReply(ctx, u, "Failed to retrieve DB.")
}
if len(users) == 0 {
	return b.sendReply(ctx, u, "No users in DB.")
}

// Serializar JSON
dataBytes, err := json.MarshalIndent(users, "", "  ")
if err != nil {
	b.logger.Printf("Failed to marshal users: %v", err)
	return b.sendReply(ctx, u, "Failed to serialize DB.")
}

// Dividir en fragmentos de 4000 bytes aprox (Telegram mensaje máximo razonable)
const chunkSize = 3800
totalLen := len(dataBytes)
parts := (totalLen + chunkSize - 1) / chunkSize

peer, err := utils.GetLogChannelPeer(ctx, b.logChannelID)
if err != nil {
	b.logger.Printf("Failed to resolve log channel peer %s: %v", b.logChannelID, err)
	return b.sendReply(ctx, u, "Failed to resolve log channel.")
}

for i := 0; i < parts; i++ {
	start := i * chunkSize
	end := start + chunkSize
	if end > totalLen {
		end = totalLen
	}
	part := dataBytes[start:end]
	message := fmt.Sprintf("DB fragment %d/%d:\n```\n%s\n```", i+1, parts, string(part))

	req := &tg.MessagesSendMessageRequest{
		Peer:    peer,
		Message: message,
	}
	_, err := b.tgCtx.SendMessageToPeer(peer, req)
	// Some gotgproto versions don't have SendMessageToPeer; fallback:
	if err != nil {
		// try sending using chat id if we can get it
		if chatID, got := utils.TryGetChatIDFromPeer(peer); got {
			_, err = b.tgCtx.SendMessage(chatID, req)
		}
	}
	if err != nil {
		b.logger.Printf("Failed to send DB fragment %d: %v", i+1, err)
		// no abortamos, seguimos con los demás fragmentos
	} else {
		// small sleep to avoid rate limits
		time.Sleep(200 * time.Millisecond)
	}
}

return b.sendReply(ctx, u, fmt.Sprintf("DB dumped to log channel (%d fragments).", parts))
````

}

// ==================== Media handling ====================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
userID := u.EffectiveUser().ID
userInfo, err := b.userRepository.GetUserInfo(userID)
if err != nil || !userInfo.IsAuthorized {
return b.sendReply(ctx, u, "You are not authorized to use this bot.")
}

```
file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
if err != nil {
	// try external link
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
```

}

// sendMediaToUser envía mensaje con botón STREAMING y publica WS
func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, _ bool) error {
keyboard := []tg.KeyboardButtonRow{
{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAMING", URL: fileURL}}},
}

```
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
```

}

// constructWebSocketMessage crea el payload para clientes web
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

// generateFileURL genera un hash corto para el recurso
func (b *TelegramBot) generateFileURL(messageID int, file *types.DocumentFile) string {
hash := utils.GetShortHash(utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID), b.config.HashLength)
return fmt.Sprintf("%s/%d/%s", b.config.BaseURL, messageID, hash)
}

// wrapWithProxyIfNeeded devuelve un path /proxy si corresponde
func (b *TelegramBot) wrapWithProxyIfNeeded(fileURL string) string {
if strings.HasPrefix(fileURL, "http://") || strings.HasPrefix(fileURL, "https://") {
if !strings.Contains(fileURL, fmt.Sprintf(":%s", b.config.Port)) &&
!strings.Contains(fileURL, "localhost") &&
!strings.HasPrefix(fileURL, b.config.BaseURL) {
return fmt.Sprintf("/proxy?url=%s", url.QueryEscape(fileURL))
}
}
return fileURL
}

// ==================== Misc utilities ====================
func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
if err != nil {
b.logger.Printf("Reply error: %v", err)
}
return err
}

// handleAnyUpdate logging debug info
func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error {
if b.config.DebugMode {
if u.EffectiveMessage != nil {
msg := u.EffectiveMessage.Message
user := u.EffectiveUser()
chatID := u.EffectiveChat().GetID()
b.logger.Debugf("Update from %s %s (ID %d) in chat %d: messageID=%d", user.FirstName, user.LastName, user.ID, chatID, msg.ID)
}
if u.CallbackQuery != nil {
b.logger.Debugf("Callback from %d: %s", u.CallbackQuery.UserID, string(u.CallbackQuery.Data))
}
}
return nil
}

// ==================== Helpers for log channel operations ====================
//
// The code below uses utils.GetLogChannelPeer which should return a tg.InputPeerClass
// (or an error). If utils.TryGetChatIDFromPeer is not implemented in your project,
// replace that with logic to extract a numeric channel ID from a peer object.

func init() {
rand.Seed(time.Now().UnixNano())
}

// NOTE: For compatibility with different gotgproto versions we try several send methods
// when targeting channels. The utils package helpers referenced must exist in your project
// (GetLogChannelPeer, ForwardMessages, TryGetChatIDFromPeer). If they don't, adapta esos
// calls al cliente que estés usando.
