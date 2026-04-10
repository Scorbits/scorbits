package explorer

import (
	"encoding/json"
	"net/http"

	"scorbits/auth"
	"scorbits/db"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func (e *Explorer) apiDMConversations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	convs, err := db.GetConversations(user.ID)
	if err != nil || convs == nil {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	json.NewEncoder(w).Encode(convs)
}

func (e *Explorer) apiDMMessages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	withHex := r.URL.Query().Get("with")
	otherID, err := bson.ObjectIDFromHex(withHex)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, 400)
		return
	}
	msgs, err := db.GetConversation(user.ID, otherID)
	if err != nil || msgs == nil {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	json.NewEncoder(w).Encode(msgs)
}

func (e *Explorer) apiDMSend(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	var req struct {
		ToID    string `json:"to_id"`
		Content string `json:"content"`
		GifURL  string `json:"gif_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	toID, err := bson.ObjectIDFromHex(req.ToID)
	if err != nil {
		http.Error(w, `{"error":"invalid to_id"}`, 400)
		return
	}
	if toID == user.ID {
		http.Error(w, `{"error":"cannot message yourself"}`, 400)
		return
	}
	canSend, err := db.CanSendDM(user.ID, toID)
	if err != nil || !canSend {
		http.Error(w, `{"error":"This user doesn't accept messages"}`, 403)
		return
	}
	toUser, err := db.GetUserByID(toID)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, 404)
		return
	}
	content := stripHTML(req.Content)
	if len([]rune(content)) > 1000 {
		content = string([]rune(content)[:1000])
	}
	if content == "" && req.GifURL == "" {
		http.Error(w, `{"error":"empty"}`, 400)
		return
	}
	fromAvatar := renderProfilePic(user, 32)
	toAvatar := renderProfilePic(toUser, 32)
	msg := &db.PrivateMessage{
		FromID:     user.ID,
		FromPseudo: user.Pseudo,
		FromAvatar: fromAvatar,
		ToID:       toID,
		ToPseudo:   toUser.Pseudo,
		ToAvatar:   toAvatar,
		Content:    content,
		GifURL:     req.GifURL,
		Read:       false,
		DeletedBy:  []string{},
	}
	if err := db.SavePrivateMessage(msg); err != nil {
		http.Error(w, `{"error":"db error"}`, 500)
		return
	}
	preview := content
	if preview == "" && req.GifURL != "" {
		preview = "GIF"
	}
	if len([]rune(preview)) > 50 {
		preview = string([]rune(preview)[:50]) + "..."
	}
	e.chatHub.SendToUser(toID.Hex(), map[string]interface{}{
		"type":        "dm_notification",
		"from_id":     user.ID.Hex(),
		"from_pseudo": user.Pseudo,
		"from_avatar": fromAvatar,
		"preview":     preview,
	})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": msg})
}

func (e *Explorer) apiDMRead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	var req struct {
		With string `json:"with"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	fromID, err := bson.ObjectIDFromHex(req.With)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, 400)
		return
	}
	db.MarkMessagesRead(fromID, user.ID)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiDMDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodDelete {
		http.Error(w, `{"error":"DELETE requis"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	msgIDHex := r.URL.Query().Get("id")
	msgID, err := bson.ObjectIDFromHex(msgIDHex)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, 400)
		return
	}
	if err := db.DeleteMessageForUser(msgID, user.ID.Hex()); err != nil {
		http.Error(w, `{"error":"db error"}`, 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiDMUnread(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		w.Write([]byte(`{"unread":0}`))
		return
	}
	count, err := db.GetUnreadCount(user.ID)
	if err != nil {
		count = 0
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"unread": count})
}

func (e *Explorer) apiDMSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Policy string `json:"policy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid"}`, 400)
			return
		}
		if req.Policy != "everyone" && req.Policy != "initiated" && req.Policy != "nobody" {
			http.Error(w, `{"error":"invalid policy"}`, 400)
			return
		}
		settings := &db.DMSettings{UserID: user.ID, Policy: req.Policy}
		if err := db.SaveDMSettings(settings); err != nil {
			http.Error(w, `{"error":"db error"}`, 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
		return
	}
	settings, err := db.GetDMSettings(user.ID)
	if err != nil || settings == nil {
		settings = &db.DMSettings{Policy: "everyone"}
	}
	json.NewEncoder(w).Encode(settings)
}

func (e *Explorer) apiUsersSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"non connecté"}`, 401)
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	users, err := db.SearchUsers(q, 10)
	if err != nil || users == nil {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	type UserResult struct {
		ID     string `json:"id"`
		Pseudo string `json:"pseudo"`
		Avatar string `json:"avatar"`
	}
	results := make([]UserResult, 0, len(users))
	for _, u := range users {
		if u.ID == user.ID {
			continue
		}
		results = append(results, UserResult{
			ID:     u.ID.Hex(),
			Pseudo: u.Pseudo,
			Avatar: renderProfilePic(u, 32),
		})
	}
	json.NewEncoder(w).Encode(results)
}
