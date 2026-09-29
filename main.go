package main

import (
	"log"
	"os"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/handlers"
	"github.com/devstr0ke/chat-mixer/middleware"
	"github.com/devstr0ke/chat-mixer/storage"
	"github.com/devstr0ke/chat-mixer/workers"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()

	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		log.Fatal("DB_URL is not set")
	}
	db.Connect(dbURL)
	defer db.DB.Close()
	db.Migrate()

	storage.Connect()
	if storage.Enabled() {
		workers.StartAttachmentCleanup(db.DB)
	}

	handlers.WSHub = handlers.NewHub()

	r := gin.Default()
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
		rooms.GET("/:room_id/messages", handlers.GetMessages)
		rooms.POST("/:room_id/attachments", handlers.UploadAttachment)
		rooms.GET("/:room_id/invitations", handlers.GetRoomInvitations)
		rooms.POST("/:room_id/invitations", handlers.InviteToRoom)
		rooms.DELETE("/:room_id/members/:user_id", handlers.RemoveMember)
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
	}

	r.GET("/attachments/:attachment_id", middleware.AuthRequiredOrCookie(), handlers.GetAttachment)
	r.GET("/avatars/:avatar_id", middleware.AuthRequiredOrCookie(), handlers.GetAvatar)

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
