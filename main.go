package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/encryption"
	"github.com/devstr0ke/chat-mixer/handlers"
	"github.com/devstr0ke/chat-mixer/middleware"
	"github.com/devstr0ke/chat-mixer/storage"
	"github.com/devstr0ke/chat-mixer/workers"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()

	// refuse to start rather than store anything unencrypted
	if err := encryption.Load(); err != nil {
		log.Fatalf("encryption: %v", err)
	}

	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		log.Fatal("DB_URL is not set")
	}
	db.Connect(dbURL)
	defer db.DB.Close()
	db.Migrate()
	// before serving, so nothing is read from rows still in plaintext
	workers.EncryptRows(db.DB)

	storage.Connect()
	if storage.Enabled() {
		workers.StartAttachmentCleanup(db.DB)
	}
	workers.StartEncryptionSweep(db.DB)

	handlers.WSHub = handlers.NewHub()

	r := gin.New()
	r.Use(gin.LoggerWithFormatter(logLine), gin.Recovery())
	r.MaxMultipartMemory = 12 << 20

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	auth := r.Group("/auth")
	{
		auth.POST("/register", handlers.Register)
		auth.POST("/login", handlers.Login)
	}

	protected := middleware.AuthRequired()

	users := r.Group("/users", protected)
	{
		users.GET("/search", handlers.SearchUsers)
		users.GET("/me", handlers.GetMe)
		users.PATCH("/me", handlers.UpdateMe)
		users.PUT("/me/avatar", handlers.UploadAvatar)
		users.DELETE("/me/avatar", handlers.DeleteAvatar)
	}

	rooms := r.Group("/rooms", protected)
	{
		rooms.POST("", handlers.CreateRoom)
		rooms.GET("/me", handlers.GetMyRooms)
		rooms.GET("/:room_id", handlers.GetRoom)
		rooms.PATCH("/:room_id", handlers.UpdateRoom)
		rooms.DELETE("/:room_id", handlers.DeleteRoom)
		rooms.PATCH("/:room_id/theme", handlers.UpdateRoomTheme)
		rooms.PUT("/:room_id/background", handlers.UploadRoomBackground)
		rooms.DELETE("/:room_id/background", handlers.DeleteRoomBackground)
		rooms.GET("/:room_id/messages", handlers.GetMessages)
		rooms.GET("/:room_id/invitations", handlers.GetRoomInvitations)
		rooms.POST("/:room_id/invitations", handlers.InviteToRoom)
		rooms.DELETE("/:room_id/members/:user_id", handlers.RemoveMember)
	}

	// the web app uploads photos and videos straight here (not via its /api proxy)
	uploadCORS := middleware.UploadCORS(os.Getenv("CORS_ORIGINS"))
	r.OPTIONS("/rooms/:room_id/attachments", uploadCORS)
	r.POST("/rooms/:room_id/attachments", uploadCORS, protected, handlers.UploadAttachment)

	r.GET("/link-preview", protected, handlers.GetLinkPreview)
	r.GET("/link-previews/images/:image_id", middleware.AuthRequiredOrCookie(), handlers.GetLinkPreviewImage)

	gifs := r.Group("/gifs", protected)
	{
		gifs.GET("/search", handlers.SearchGifs)
		gifs.GET("/trending", handlers.TrendingGifs)
	}

	stickers := r.Group("/stickers", protected)
	{
		stickers.GET("", handlers.ListMyStickers)
		stickers.POST("", handlers.CreateSticker)
		stickers.DELETE("/:sticker_id", handlers.DeleteSticker)
	}

	invitations := r.Group("/invitations", protected)
	{
		invitations.GET("", handlers.GetMyInvitations)
		invitations.POST("/:invitation_id/accept", handlers.AcceptInvitation)
		invitations.DELETE("/:invitation_id", handlers.DeleteInvitation)
	}

	messages := r.Group("/messages", protected)
	{
		messages.POST("/:message_id/reactions", handlers.ReactToMessage)
		messages.DELETE("/:message_id/reactions", handlers.RemoveReaction)
		messages.PATCH("/:message_id", handlers.EditMessage)
		messages.GET("/:message_id/edits", handlers.GetMessageEdits)
		messages.POST("/:message_id/open", handlers.OpenViewOnce)
		messages.DELETE("/:message_id", handlers.DeleteMessage)
	}

	r.GET("/attachments/:attachment_id", middleware.AuthRequiredOrCookie(), handlers.GetAttachment)
	r.GET("/attachments/:attachment_id/poster", middleware.AuthRequiredOrCookie(), handlers.GetAttachmentPoster)
	r.GET("/avatars/:avatar_id", middleware.AuthRequiredOrCookie(), handlers.GetAvatar)
	r.GET("/stickers/:sticker_id/image", middleware.AuthRequiredOrCookie(), handlers.GetStickerImage)
	r.GET("/rooms/:room_id/background/:image_id", middleware.AuthRequiredOrCookie(), handlers.GetRoomBackground)

	r.GET("/ws/notifications", protected, handlers.HandleNotificationWS)
	r.GET("/ws/:room_id", protected, handlers.HandleWebSocket)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("server: starting on :%s", port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// logLine is gin's usual request log line without the query string, which
// can hold what users shared or searched (link preview URLs, GIF searches).
func logLine(p gin.LogFormatterParams) string {
	path, _, _ := strings.Cut(p.Path, "?")
	line := fmt.Sprintf("[GIN] %s | %3d | %13v | %15s | %-7s %q\n",
		p.TimeStamp.Format("2006/01/02 - 15:04:05"), p.StatusCode, p.Latency.Truncate(time.Microsecond),
		p.ClientIP, p.Method, path)
	if p.ErrorMessage != "" {
		line += p.ErrorMessage
	}
	return line
}
