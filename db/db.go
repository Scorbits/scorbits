package db

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type User struct {
	ID           bson.ObjectID `bson:"_id,omitempty" json:"id"`
	Email        string        `bson:"email" json:"email"`
	PasswordHash string        `bson:"password_hash" json:"-"`
	Pseudo       string        `bson:"pseudo" json:"pseudo"`
	Address      string        `bson:"address" json:"address"`
	PubKey       string        `bson:"pub_key" json:"pub_key"`
	ProfilePic   string        `bson:"profile_pic" json:"profile_pic"`
	Verified     bool          `bson:"verified" json:"verified"`
	VerifyToken  string        `bson:"verify_token" json:"-"`
	IsScammer    bool          `bson:"is_scammer" json:"is_scammer"`
	CreatedAt    time.Time     `bson:"created_at" json:"created_at"`
	UpdatedAt    time.Time     `bson:"updated_at" json:"updated_at"`
}

// ArticleReaction — likes/dislikes sur les articles de news
type ArticleReaction struct {
	ID         bson.ObjectID `bson:"_id,omitempty" json:"id"`
	ArticleURL string        `bson:"article_url" json:"article_url"`
	Likes      int           `bson:"likes" json:"likes"`
	Dislikes   int           `bson:"dislikes" json:"dislikes"`
}

var client *mongo.Client

func Connect() error {
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		return fmt.Errorf("MONGODB_URI non défini")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var err error
	client, err = mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return fmt.Errorf("erreur connexion MongoDB: %w", err)
	}
	if err = client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("MongoDB inaccessible: %w", err)
	}
	fmt.Println("[DB] Connecté à MongoDB Atlas")

	col := Collection("users")
	col.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true)})
	col.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "pseudo", Value: 1}}, Options: options.Index().SetUnique(true)})
	col.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "verify_token", Value: 1}}})

	Collection("reactions").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "user_id", Value: 1}, {Key: "target_id", Value: 1}, {Key: "target_type", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	return nil
}

func Collection(name string) *mongo.Collection {
	dbName := os.Getenv("MONGODB_DB")
	if dbName == "" {
		dbName = "scorbits"
	}
	return client.Database(dbName).Collection(name)
}

func Disconnect() {
	if client != nil {
		client.Disconnect(context.Background())
	}
}

// ─── USERS ────────────────────────────────────────────────────────────────────

func CreateUser(u *User) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u.ID = bson.NewObjectID()
	u.CreatedAt = time.Now()
	u.UpdatedAt = time.Now()
	_, err := Collection("users").InsertOne(ctx, u)
	return err
}

func GetUserByEmail(email string) (*User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var u User
	err := Collection("users").FindOne(ctx, bson.M{"email": email}).Decode(&u)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func GetUserByID(id bson.ObjectID) (*User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var u User
	err := Collection("users").FindOne(ctx, bson.M{"_id": id}).Decode(&u)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func GetUserByPseudo(pseudo string) (*User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var u User
	err := Collection("users").FindOne(ctx, bson.M{"pseudo": pseudo}).Decode(&u)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func GetUserByAddress(address string) (*User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var u User
	err := Collection("users").FindOne(ctx, bson.M{"address": address}).Decode(&u)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func GetUserByVerifyToken(token string) (*User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var u User
	err := Collection("users").FindOne(ctx, bson.M{"verify_token": token}).Decode(&u)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func VerifyUser(id bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("users").UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"verified": true, "verify_token": "", "updated_at": time.Now()}},
	)
	return err
}

func UpdateUserProfilePic(id bson.ObjectID, url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("users").UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"profile_pic": url, "updated_at": time.Now()}},
	)
	return err
}

func UpdateUserPassword(id bson.ObjectID, hash string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("users").UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"password_hash": hash, "updated_at": time.Now()}},
	)
	return err
}

func PseudoExists(pseudo string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	count, _ := Collection("users").CountDocuments(ctx, bson.M{"pseudo": pseudo})
	return count > 0
}

func EmailExists(email string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	count, _ := Collection("users").CountDocuments(ctx, bson.M{"email": email})
	return count > 0
}

func MarkUserScammer(id bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("users").UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"is_scammer": true, "updated_at": time.Now()}},
	)
	return err
}

func GetArticleReaction(articleURL string) (*ArticleReaction, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var r ArticleReaction
	err := Collection("article_reactions").FindOne(ctx, bson.M{"article_url": articleURL}).Decode(&r)
	if err != nil {
		return &ArticleReaction{ArticleURL: articleURL}, nil
	}
	return &r, nil
}

func UpsertArticleReaction(articleURL, reaction string) (*ArticleReaction, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inc := bson.M{}
	if reaction == "like" {
		inc["likes"] = 1
	} else {
		inc["dislikes"] = 1
	}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	var r ArticleReaction
	err := Collection("article_reactions").FindOneAndUpdate(ctx,
		bson.M{"article_url": articleURL},
		bson.M{"$inc": inc, "$setOnInsert": bson.M{"article_url": articleURL}},
		opts,
	).Decode(&r)
	return &r, err
}

// ─── POSTS ────────────────────────────────────────────────────────────────────

type Post struct {
	ID           bson.ObjectID `bson:"_id,omitempty" json:"id"`
	UserID       bson.ObjectID `bson:"user_id" json:"user_id"`
	Pseudo       string        `bson:"pseudo" json:"pseudo"`
	AvatarSVG    string        `bson:"avatar_svg" json:"avatar_svg"`
	Content      string        `bson:"content" json:"content"`
	ImageURL     string        `bson:"image_url,omitempty" json:"image_url,omitempty"`
	GifURL       string        `bson:"gif_url,omitempty" json:"gif_url,omitempty"`
	Likes        int           `bson:"likes" json:"likes"`
	Dislikes     int           `bson:"dislikes" json:"dislikes"`
	UserReaction string        `bson:"-" json:"user_reaction,omitempty"`
	CreatedAt    time.Time     `bson:"created_at" json:"created_at"`
}

type Comment struct {
	ID           bson.ObjectID `bson:"_id,omitempty" json:"id"`
	PostID       bson.ObjectID `bson:"post_id" json:"post_id"`
	UserID       bson.ObjectID `bson:"user_id" json:"user_id"`
	Pseudo       string        `bson:"pseudo" json:"pseudo"`
	AvatarSVG    string        `bson:"avatar_svg" json:"avatar_svg"`
	Content      string        `bson:"content" json:"content"`
	UserReaction string        `bson:"-" json:"user_reaction,omitempty"`
	CreatedAt    time.Time     `bson:"created_at" json:"created_at"`
}

// ─── REACTIONS ────────────────────────────────────────────────────────────────

type Reaction struct {
	ID           bson.ObjectID `bson:"_id,omitempty" json:"id"`
	UserID       bson.ObjectID `bson:"user_id" json:"user_id"`
	TargetID     bson.ObjectID `bson:"target_id" json:"target_id"`
	TargetType   string        `bson:"target_type" json:"target_type"`
	ReactionType string        `bson:"reaction_type" json:"reaction_type"`
	CreatedAt    time.Time     `bson:"created_at" json:"created_at"`
}


func CreatePost(p *Post) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.ID = bson.NewObjectID()
	p.CreatedAt = time.Now()
	_, err := Collection("posts").InsertOne(ctx, p)
	return err
}

func GetPosts(limit int, userID bson.ObjectID) ([]*Post, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(int64(limit))
	cur, err := Collection("posts").Find(ctx, bson.M{}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var posts []*Post
	cur.All(ctx, &posts)
	if !userID.IsZero() && len(posts) > 0 {
		postIDs := make([]bson.ObjectID, len(posts))
		for i, p := range posts {
			postIDs[i] = p.ID
		}
		rCur, rErr := Collection("reactions").Find(ctx, bson.M{
			"user_id":     userID,
			"target_id":   bson.M{"$in": postIDs},
			"target_type": "post",
		})
		if rErr == nil {
			var reactions []*Reaction
			rCur.All(ctx, &reactions)
			rCur.Close(ctx)
			rMap := map[bson.ObjectID]string{}
			for _, r := range reactions {
				rMap[r.TargetID] = r.ReactionType
			}
			for _, p := range posts {
				p.UserReaction = rMap[p.ID]
			}
		}
	}
	return posts, nil
}

// TogglePostReaction handles unique per-user reactions with toggle logic.
func TogglePostReaction(postID, userID bson.ObjectID, reaction string) (*Post, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	col := Collection("reactions")
	var existing Reaction
	err := col.FindOne(ctx, bson.M{"user_id": userID, "target_id": postID, "target_type": "post"}).Decode(&existing)
	inc := bson.M{}
	activeReaction := reaction
	if err != nil {
		// No existing reaction — create new
		col.InsertOne(ctx, &Reaction{
			ID: bson.NewObjectID(), UserID: userID, TargetID: postID,
			TargetType: "post", ReactionType: reaction, CreatedAt: time.Now(),
		})
		if reaction == "like" {
			inc["likes"] = 1
		} else {
			inc["dislikes"] = 1
		}
	} else if existing.ReactionType == reaction {
		// Same reaction — toggle off
		col.DeleteOne(ctx, bson.M{"_id": existing.ID})
		if reaction == "like" {
			inc["likes"] = -1
		} else {
			inc["dislikes"] = -1
		}
		activeReaction = ""
	} else {
		// Different reaction — switch
		col.UpdateOne(ctx, bson.M{"_id": existing.ID}, bson.M{"$set": bson.M{"reaction_type": reaction}})
		if reaction == "like" {
			inc["likes"] = 1
			inc["dislikes"] = -1
		} else {
			inc["likes"] = -1
			inc["dislikes"] = 1
		}
	}
	upOpts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var p Post
	fErr := Collection("posts").FindOneAndUpdate(ctx, bson.M{"_id": postID}, bson.M{"$inc": inc}, upOpts).Decode(&p)
	if fErr != nil {
		return nil, activeReaction, fErr
	}
	p.UserReaction = activeReaction
	return &p, activeReaction, nil
}

func DeletePost(postID, userID bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := Collection("posts").DeleteOne(ctx, bson.M{"_id": postID, "user_id": userID})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return fmt.Errorf("post introuvable ou non autorisé")
	}
	Collection("comments").DeleteMany(ctx, bson.M{"post_id": postID})
	Collection("reactions").DeleteMany(ctx, bson.M{"target_id": postID, "target_type": "post"})
	return nil
}

func AddComment(c *Comment) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.ID = bson.NewObjectID()
	c.CreatedAt = time.Now()
	_, err := Collection("comments").InsertOne(ctx, c)
	return err
}

func GetComments(postID bson.ObjectID) ([]*Comment, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})
	cur, err := Collection("comments").Find(ctx, bson.M{"post_id": postID}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var comments []*Comment
	cur.All(ctx, &comments)
	return comments, nil
}

func DeleteComment(commentID, userID bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := Collection("comments").DeleteOne(ctx, bson.M{"_id": commentID, "user_id": userID})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return fmt.Errorf("commentaire introuvable ou non autorisé")
	}
	return nil
}

func AdminDeletePost(postID bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	Collection("posts").DeleteOne(ctx, bson.M{"_id": postID})
	Collection("comments").DeleteMany(ctx, bson.M{"post_id": postID})
	Collection("reactions").DeleteMany(ctx, bson.M{"target_id": postID, "target_type": "post"})
	return nil
}

func AdminDeleteComment(commentID bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	Collection("comments").DeleteOne(ctx, bson.M{"_id": commentID})
	return nil
}

func AdminDeleteChatMessage(msgID bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	Collection("chat_messages").DeleteOne(ctx, bson.M{"_id": msgID})
	return nil
}

func GetPostByID(id bson.ObjectID) (*Post, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var p Post
	err := Collection("posts").FindOne(ctx, bson.M{"_id": id}).Decode(&p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ─── CHAT ─────────────────────────────────────────────────────────────────────

type SharedPostPreview struct {
	PostID       string `bson:"post_id" json:"post_id"`
	AuthorPseudo string `bson:"author_pseudo" json:"author_pseudo"`
	AuthorAvatar string `bson:"author_avatar" json:"author_avatar"`
	Content      string `bson:"content" json:"content"`
	ImageURL     string `bson:"image_url,omitempty" json:"image_url,omitempty"`
	CreatedAt    string `bson:"created_at" json:"created_at"`
}

type ChatMessage struct {
	ID         bson.ObjectID       `bson:"_id,omitempty" json:"id"`
	UserID     bson.ObjectID       `bson:"user_id" json:"user_id"`
	Pseudo     string              `bson:"pseudo" json:"pseudo"`
	AvatarURL  string              `bson:"avatar_url" json:"avatar_url"`
	IsMiner    bool                `bson:"is_miner" json:"is_miner"`
	Content    string              `bson:"content" json:"content"`
	GifURL     string              `bson:"gif_url,omitempty" json:"gif_url,omitempty"`
	Reactions  map[string][]string `bson:"reactions" json:"reactions"`
	SharedPost *SharedPostPreview  `bson:"shared_post,omitempty" json:"shared_post,omitempty"`
	CreatedAt  time.Time           `bson:"created_at" json:"created_at"`
}

type ChatBlock struct {
	ID        bson.ObjectID `bson:"_id,omitempty" json:"id"`
	BlockerID bson.ObjectID `bson:"blocker_id" json:"blocker_id"`
	BlockedID bson.ObjectID `bson:"blocked_id" json:"blocked_id"`
	CreatedAt time.Time     `bson:"created_at" json:"created_at"`
}

type ChatMute struct {
	ID        bson.ObjectID `bson:"_id,omitempty" json:"id"`
	UserID    bson.ObjectID `bson:"user_id" json:"user_id"`
	MutedBy   bson.ObjectID `bson:"muted_by" json:"muted_by"`
	ExpiresAt time.Time     `bson:"expires_at" json:"expires_at"`
}

func SaveChatMessage(msg *ChatMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg.ID = bson.NewObjectID()
	if msg.Reactions == nil {
		msg.Reactions = make(map[string][]string)
	}
	msg.CreatedAt = time.Now()
	_, err := Collection("chat_messages").InsertOne(ctx, msg)
	return err
}

func GetChatMessages() ([]*ChatMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cutoff := time.Now().Add(-24 * time.Hour)
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})
	cur, err := Collection("chat_messages").Find(ctx, bson.M{"created_at": bson.M{"$gte": cutoff}}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var msgs []*ChatMessage
	cur.All(ctx, &msgs)
	return msgs, nil
}

func GetChatMessageByID(id bson.ObjectID) (*ChatMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var msg ChatMessage
	err := Collection("chat_messages").FindOne(ctx, bson.M{"_id": id}).Decode(&msg)
	if err != nil {
		return nil, err
	}
	return &msg, nil
}

func DeleteOldChatMessages() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cutoff := time.Now().Add(-24 * time.Hour)
	_, err := Collection("chat_messages").DeleteMany(ctx, bson.M{"created_at": bson.M{"$lt": cutoff}})
	return err
}

// GetUserReactionOnMessage returns the emoji the user has already reacted with on this message, or "" if none.
func GetUserReactionOnMessage(messageID bson.ObjectID, userID string) (string, error) {
	msg, err := GetChatMessageByID(messageID)
	if err != nil {
		return "", err
	}
	for emoji, users := range msg.Reactions {
		for _, uid := range users {
			if uid == userID {
				return emoji, nil
			}
		}
	}
	return "", nil
}

func AddReaction(messageID bson.ObjectID, emoji string, userID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("chat_messages").UpdateOne(ctx,
		bson.M{"_id": messageID},
		bson.M{"$addToSet": bson.M{"reactions." + emoji: userID}},
	)
	return err
}

func RemoveReaction(messageID bson.ObjectID, emoji string, userID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("chat_messages").UpdateOne(ctx,
		bson.M{"_id": messageID},
		bson.M{"$pull": bson.M{"reactions." + emoji: userID}},
	)
	return err
}

func BlockUser(blockerID, blockedID bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	block := &ChatBlock{
		ID:        bson.NewObjectID(),
		BlockerID: blockerID,
		BlockedID: blockedID,
		CreatedAt: time.Now(),
	}
	_, err := Collection("chat_blocks").InsertOne(ctx, block)
	return err
}

func UnblockUser(blockerID, blockedID bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("chat_blocks").DeleteOne(ctx, bson.M{"blocker_id": blockerID, "blocked_id": blockedID})
	return err
}

func GetBlockedUsers(blockerID bson.ObjectID) ([]bson.ObjectID, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cur, err := Collection("chat_blocks").Find(ctx, bson.M{"blocker_id": blockerID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var blocks []*ChatBlock
	cur.All(ctx, &blocks)
	ids := make([]bson.ObjectID, len(blocks))
	for i, b := range blocks {
		ids[i] = b.BlockedID
	}
	return ids, nil
}

func MuteUser(userID, mutedBy bson.ObjectID, duration time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mute := &ChatMute{
		ID:        bson.NewObjectID(),
		UserID:    userID,
		MutedBy:   mutedBy,
		ExpiresAt: time.Now().Add(duration),
	}
	opts := options.UpdateOne().SetUpsert(true)
	_, err := Collection("chat_mutes").UpdateOne(ctx,
		bson.M{"user_id": userID},
		bson.M{"$set": mute},
		opts,
	)
	return err
}

func IsUserMuted(userID bson.ObjectID) (bool, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mute ChatMute
	err := Collection("chat_mutes").FindOne(ctx, bson.M{"user_id": userID}).Decode(&mute)
	if err != nil {
		return false, time.Time{}, nil
	}
	if time.Now().After(mute.ExpiresAt) {
		Collection("chat_mutes").DeleteOne(ctx, bson.M{"_id": mute.ID})
		return false, time.Time{}, nil
	}
	return true, mute.ExpiresAt, nil
}

// ─── PRIVATE MESSAGES ─────────────────────────────────────────────────────────

type PrivateMessage struct {
	ID         bson.ObjectID `bson:"_id,omitempty" json:"id"`
	FromID     bson.ObjectID `bson:"from_id" json:"from_id"`
	FromPseudo string        `bson:"from_pseudo" json:"from_pseudo"`
	FromAvatar string        `bson:"from_avatar" json:"from_avatar"`
	ToID       bson.ObjectID `bson:"to_id" json:"to_id"`
	ToPseudo   string        `bson:"to_pseudo" json:"to_pseudo"`
	ToAvatar   string        `bson:"to_avatar" json:"to_avatar"`
	Content    string        `bson:"content" json:"content"`
	GifURL     string        `bson:"gif_url,omitempty" json:"gif_url,omitempty"`
	Read       bool          `bson:"read" json:"read"`
	ReadAt     *time.Time    `bson:"read_at,omitempty" json:"read_at,omitempty"`
	DeletedBy  []string      `bson:"deleted_by" json:"deleted_by"`
	CreatedAt  time.Time     `bson:"created_at" json:"created_at"`
}

type DMSettings struct {
	ID     bson.ObjectID `bson:"_id,omitempty" json:"id"`
	UserID bson.ObjectID `bson:"user_id" json:"user_id"`
	Policy string        `bson:"policy" json:"policy"`
}

type Conversation struct {
	OtherUserID   string    `json:"other_user_id"`
	OtherPseudo   string    `json:"other_pseudo"`
	OtherAvatar   string    `json:"other_avatar"`
	LastMessage   string    `json:"last_message"`
	LastMessageAt time.Time `json:"last_message_at"`
	UnreadCount   int       `json:"unread_count"`
}

func SavePrivateMessage(msg *PrivateMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg.ID = bson.NewObjectID()
	msg.CreatedAt = time.Now()
	if msg.DeletedBy == nil {
		msg.DeletedBy = []string{}
	}
	_, err := Collection("private_messages").InsertOne(ctx, msg)
	return err
}

func GetConversation(userID, otherID bson.ObjectID) ([]*PrivateMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	filter := bson.M{
		"$or": []bson.M{
			{"from_id": userID, "to_id": otherID},
			{"from_id": otherID, "to_id": userID},
		},
		"deleted_by": bson.M{"$nin": []string{userID.Hex()}},
	}
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})
	cur, err := Collection("private_messages").Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var msgs []*PrivateMessage
	cur.All(ctx, &msgs)
	if msgs == nil {
		msgs = []*PrivateMessage{}
	}
	return msgs, nil
}

func GetConversations(userID bson.ObjectID) ([]*Conversation, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	filter := bson.M{
		"$or": []bson.M{
			{"from_id": userID},
			{"to_id": userID},
		},
		"deleted_by": bson.M{"$nin": []string{userID.Hex()}},
	}
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	cur, err := Collection("private_messages").Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var msgs []*PrivateMessage
	cur.All(ctx, &msgs)

	seen := map[string]bool{}
	var convs []*Conversation
	for _, m := range msgs {
		var otherID bson.ObjectID
		var otherPseudo, otherAvatar string
		if m.FromID == userID {
			otherID = m.ToID
			otherPseudo = m.ToPseudo
			otherAvatar = m.ToAvatar
		} else {
			otherID = m.FromID
			otherPseudo = m.FromPseudo
			otherAvatar = m.FromAvatar
		}
		key := otherID.Hex()
		if seen[key] {
			continue
		}
		seen[key] = true
		preview := m.Content
		if m.GifURL != "" && preview == "" {
			preview = "GIF"
		}
		if len([]rune(preview)) > 40 {
			preview = string([]rune(preview)[:40]) + "..."
		}
		unread := 0
		for _, m2 := range msgs {
			if m2.FromID == otherID && m2.ToID == userID && !m2.Read {
				unread++
			}
		}
		convs = append(convs, &Conversation{
			OtherUserID:   key,
			OtherPseudo:   otherPseudo,
			OtherAvatar:   otherAvatar,
			LastMessage:   preview,
			LastMessageAt: m.CreatedAt,
			UnreadCount:   unread,
		})
	}
	if convs == nil {
		convs = []*Conversation{}
	}
	return convs, nil
}

func GetUnreadCount(userID bson.ObjectID) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	count, err := Collection("private_messages").CountDocuments(ctx, bson.M{
		"to_id":      userID,
		"read":       false,
		"deleted_by": bson.M{"$nin": []string{userID.Hex()}},
	})
	if err != nil {
		return 0, err
	}
	groupCount, _ := GetGroupUnreadCount(userID)
	return int(count) + groupCount, nil
}

func MarkMessagesRead(fromID, toID bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now()
	_, err := Collection("private_messages").UpdateMany(ctx,
		bson.M{"from_id": fromID, "to_id": toID, "read": false},
		bson.M{"$set": bson.M{"read": true, "read_at": now}},
	)
	return err
}

func DeleteMessageForUser(messageID bson.ObjectID, userID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("private_messages").UpdateOne(ctx,
		bson.M{"_id": messageID},
		bson.M{"$addToSet": bson.M{"deleted_by": userID}},
	)
	return err
}

func GetDMSettings(userID bson.ObjectID) (*DMSettings, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var s DMSettings
	err := Collection("dm_settings").FindOne(ctx, bson.M{"user_id": userID}).Decode(&s)
	if err != nil {
		return &DMSettings{UserID: userID, Policy: "everyone"}, nil
	}
	return &s, nil
}

func SaveDMSettings(settings *DMSettings) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := options.UpdateOne().SetUpsert(true)
	_, err := Collection("dm_settings").UpdateOne(ctx,
		bson.M{"user_id": settings.UserID},
		bson.M{"$set": settings},
		opts,
	)
	return err
}

func SearchUsers(query string, limit int) ([]*User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	filter := bson.M{"pseudo": bson.M{"$regex": query, "$options": "i"}}
	opts := options.Find().SetLimit(int64(limit))
	cur, err := Collection("users").Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var users []*User
	cur.All(ctx, &users)
	if users == nil {
		users = []*User{}
	}
	return users, nil
}

func CanSendDM(fromID, toID bson.ObjectID) (bool, error) {
	settings, err := GetDMSettings(toID)
	if err != nil {
		return true, nil
	}
	switch settings.Policy {
	case "nobody":
		return false, nil
	case "initiated":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		count, err := Collection("private_messages").CountDocuments(ctx, bson.M{
			"from_id": toID,
			"to_id":   fromID,
		})
		if err != nil {
			return false, err
		}
		return count > 0, nil
	default:
		return true, nil
	}
}

// ─── DM GROUPS ─────────────────────────────────────────────────────────────

type GroupMemberInfo struct {
	UserID bson.ObjectID `bson:"user_id" json:"user_id"`
	Pseudo string        `bson:"pseudo" json:"pseudo"`
	Avatar string        `bson:"avatar" json:"avatar"`
}

type DMGroup struct {
	ID          bson.ObjectID     `bson:"_id,omitempty" json:"id"`
	Name        string            `bson:"name" json:"name"`
	AvatarURL   string            `bson:"avatar_url,omitempty" json:"avatar_url,omitempty"`
	CreatedBy   bson.ObjectID     `bson:"created_by" json:"created_by"`
	Members     []bson.ObjectID   `bson:"members" json:"members"`
	MemberInfos []GroupMemberInfo `bson:"member_infos" json:"member_infos"`
	CreatedAt   time.Time         `bson:"created_at" json:"created_at"`
}

type GroupMessage struct {
	ID         bson.ObjectID `bson:"_id,omitempty" json:"id"`
	GroupID    bson.ObjectID `bson:"group_id" json:"group_id"`
	FromID     bson.ObjectID `bson:"from_id" json:"from_id"`
	FromPseudo string        `bson:"from_pseudo" json:"from_pseudo"`
	FromAvatar string        `bson:"from_avatar" json:"from_avatar"`
	Content    string        `bson:"content" json:"content"`
	GifURL     string        `bson:"gif_url,omitempty" json:"gif_url,omitempty"`
	ReadBy     []string      `bson:"read_by" json:"read_by"`
	CreatedAt  time.Time     `bson:"created_at" json:"created_at"`
}

func CreateDMGroup(group *DMGroup) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	group.ID = bson.NewObjectID()
	group.CreatedAt = time.Now()
	_, err := Collection("dm_groups").InsertOne(ctx, group)
	return err
}

func GetDMGroup(groupID bson.ObjectID) (*DMGroup, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var g DMGroup
	err := Collection("dm_groups").FindOne(ctx, bson.M{"_id": groupID}).Decode(&g)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func GetUserGroups(userID bson.ObjectID) ([]*DMGroup, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cur, err := Collection("dm_groups").Find(ctx, bson.M{"members": userID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var groups []*DMGroup
	cur.All(ctx, &groups)
	if groups == nil {
		groups = []*DMGroup{}
	}
	return groups, nil
}

func AddGroupMember(groupID, userID bson.ObjectID, pseudo, avatar string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info := GroupMemberInfo{UserID: userID, Pseudo: pseudo, Avatar: avatar}
	_, err := Collection("dm_groups").UpdateOne(ctx,
		bson.M{"_id": groupID, "members": bson.M{"$ne": userID}},
		bson.M{
			"$addToSet": bson.M{"members": userID},
			"$push":     bson.M{"member_infos": info},
		},
	)
	return err
}

func RemoveGroupMember(groupID, userID bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("dm_groups").UpdateOne(ctx,
		bson.M{"_id": groupID},
		bson.M{
			"$pull": bson.M{
				"members":      userID,
				"member_infos": bson.M{"user_id": userID},
			},
		},
	)
	return err
}

func SaveGroupMessage(msg *GroupMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg.ID = bson.NewObjectID()
	msg.CreatedAt = time.Now()
	if msg.ReadBy == nil {
		msg.ReadBy = []string{msg.FromID.Hex()}
	}
	_, err := Collection("group_messages").InsertOne(ctx, msg)
	return err
}

func GetGroupMessages(groupID bson.ObjectID) ([]*GroupMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}).SetLimit(100)
	cur, err := Collection("group_messages").Find(ctx, bson.M{"group_id": groupID}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var msgs []*GroupMessage
	cur.All(ctx, &msgs)
	if msgs == nil {
		msgs = []*GroupMessage{}
	}
	return msgs, nil
}

func MarkGroupMessagesRead(groupID bson.ObjectID, userID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("group_messages").UpdateMany(ctx,
		bson.M{"group_id": groupID, "read_by": bson.M{"$nin": []string{userID}}},
		bson.M{"$addToSet": bson.M{"read_by": userID}},
	)
	return err
}

func GetGroupUnreadCount(userID bson.ObjectID) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	userIDHex := userID.Hex()
	cur, err := Collection("dm_groups").Find(ctx, bson.M{"members": userID})
	if err != nil {
		return 0, err
	}
	var groups []*DMGroup
	cur.All(ctx, &groups)
	cur.Close(ctx)
	if len(groups) == 0 {
		return 0, nil
	}
	groupIDs := make([]bson.ObjectID, len(groups))
	for i, g := range groups {
		groupIDs[i] = g.ID
	}
	count, err := Collection("group_messages").CountDocuments(ctx, bson.M{
		"group_id": bson.M{"$in": groupIDs},
		"from_id":  bson.M{"$ne": userID},
		"read_by":  bson.M{"$nin": []string{userIDHex}},
	})
	return int(count), err
}

// ─── NOTIFICATIONS ────────────────────────────────────────────────────────────

type Notification struct {
	ID             bson.ObjectID `bson:"_id,omitempty" json:"_id"`
	UserID         bson.ObjectID `bson:"user_id" json:"userID"`
	FromID         bson.ObjectID `bson:"from_id" json:"fromID"`
	FromPseudo     string        `bson:"from_pseudo" json:"fromUsername"`
	FromAvatarSVG  string        `bson:"from_avatar_svg" json:"fromAvatarSVG"`
	PostID         bson.ObjectID `bson:"post_id" json:"postID"`
	CommentPreview string        `bson:"comment_preview" json:"commentPreview"`
	Read           bool          `bson:"read" json:"read"`
	CreatedAt      time.Time     `bson:"created_at" json:"createdAt"`
}

func CreateNotification(n *Notification) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n.ID = bson.NewObjectID()
	n.CreatedAt = time.Now()
	_, err := Collection("notifications").InsertOne(ctx, n)
	return err
}

func GetNotifications(userID bson.ObjectID) ([]*Notification, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(50)
	cur, err := Collection("notifications").Find(ctx, bson.M{"user_id": userID}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var notifs []*Notification
	cur.All(ctx, &notifs)
	return notifs, nil
}

func MarkAllNotificationsRead(userID bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("notifications").UpdateMany(ctx,
		bson.M{"user_id": userID, "read": false},
		bson.M{"$set": bson.M{"read": true}},
	)
	return err
}

func MarkNotificationRead(notifID, userID bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("notifications").UpdateOne(ctx,
		bson.M{"_id": notifID, "user_id": userID},
		bson.M{"$set": bson.M{"read": true}},
	)
	return err
}

func CountPostComments(postID bson.ObjectID) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return Collection("comments").CountDocuments(ctx, bson.M{"post_id": postID})
}

func UpdateGroupName(groupID bson.ObjectID, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Collection("dm_groups").UpdateOne(ctx,
		bson.M{"_id": groupID},
		bson.M{"$set": bson.M{"name": name}},
	)
	return err
}
