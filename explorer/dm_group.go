package explorer

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"scorbits/auth"
	"scorbits/db"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// groupListItem is the JSON response shape for /api/dm/group/list
type groupListItem struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	AvatarURL     string               `json:"avatar_url,omitempty"`
	CreatedBy     string               `json:"created_by"`
	MemberCount   int                  `json:"member_count"`
	MemberInfos   []groupMemberInfoJSON `json:"member_infos"`
	LastMessage   string               `json:"last_message"`
	LastMessageAt time.Time            `json:"last_message_at"`
	UnreadCount   int                  `json:"unread_count"`
	IsCreator     bool                 `json:"is_creator"`
}

type groupMemberInfoJSON struct {
	UserID string `json:"user_id"`
	Pseudo string `json:"pseudo"`
	Avatar string `json:"avatar"`
}

type groupMessageJSON struct {
	ID         string    `json:"id"`
	GroupID    string    `json:"group_id"`
	FromID     string    `json:"from_id"`
	FromPseudo string    `json:"from_pseudo"`
	FromAvatar string    `json:"from_avatar"`
	Content    string    `json:"content"`
	GifURL     string    `json:"gif_url,omitempty"`
	ReadBy     []string  `json:"read_by"`
	CreatedAt  time.Time `json:"created_at"`
}

func (e *Explorer) apiDMGroupCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST required"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"not logged in"}`, 401)
		return
	}
	var req struct {
		Name      string   `json:"name"`
		MemberIDs []string `json:"member_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, `{"error":"group name required"}`, 400)
		return
	}
	if len(req.MemberIDs) < 2 {
		http.Error(w, `{"error":"at least 2 members required"}`, 400)
		return
	}
	if len(req.MemberIDs) > 49 {
		http.Error(w, `{"error":"max 50 members"}`, 400)
		return
	}

	members := []bson.ObjectID{user.ID}
	memberInfos := []db.GroupMemberInfo{{
		UserID: user.ID,
		Pseudo: user.Pseudo,
		Avatar: renderProfilePic(user, 32),
	}}
	seen := map[string]bool{user.ID.Hex(): true}

	for _, idStr := range req.MemberIDs {
		uid, err := bson.ObjectIDFromHex(idStr)
		if err != nil || seen[uid.Hex()] {
			continue
		}
		seen[uid.Hex()] = true
		u, err := db.GetUserByID(uid)
		if err != nil {
			continue
		}
		members = append(members, uid)
		memberInfos = append(memberInfos, db.GroupMemberInfo{
			UserID: uid,
			Pseudo: u.Pseudo,
			Avatar: renderProfilePic(u, 32),
		})
	}

	group := &db.DMGroup{
		Name:        name,
		CreatedBy:   user.ID,
		Members:     members,
		MemberInfos: memberInfos,
	}
	if err := db.CreateDMGroup(group); err != nil {
		http.Error(w, `{"error":"db error"}`, 500)
		return
	}
	for _, info := range memberInfos {
		if info.UserID == user.ID {
			continue
		}
		e.chatHub.SendToUser(info.UserID.Hex(), map[string]interface{}{
			"type":        "group_invite",
			"group_id":    group.ID.Hex(),
			"group_name":  group.Name,
			"from_pseudo": user.Pseudo,
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "group_id": group.ID.Hex()})
}

func (e *Explorer) apiDMGroupList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"not logged in"}`, 401)
		return
	}
	groups, err := db.GetUserGroups(user.ID)
	if err != nil || groups == nil {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	userIDHex := user.ID.Hex()
	result := make([]groupListItem, 0, len(groups))
	for _, g := range groups {
		msgs, _ := db.GetGroupMessages(g.ID)
		lastMsg := ""
		var lastMsgAt time.Time
		unread := 0
		if len(msgs) > 0 {
			m := msgs[len(msgs)-1]
			lastMsg = m.Content
			if lastMsg == "" && m.GifURL != "" {
				lastMsg = "GIF"
			}
			if len([]rune(lastMsg)) > 40 {
				lastMsg = string([]rune(lastMsg)[:40]) + "..."
			}
			lastMsgAt = m.CreatedAt
			for _, m2 := range msgs {
				if m2.FromID == user.ID {
					continue
				}
				read := false
				for _, rid := range m2.ReadBy {
					if rid == userIDHex {
						read = true
						break
					}
				}
				if !read {
					unread++
				}
			}
		} else {
			lastMsgAt = g.CreatedAt
		}
		infos := make([]groupMemberInfoJSON, len(g.MemberInfos))
		for i, mi := range g.MemberInfos {
			infos[i] = groupMemberInfoJSON{
				UserID: mi.UserID.Hex(),
				Pseudo: mi.Pseudo,
				Avatar: mi.Avatar,
			}
		}
		result = append(result, groupListItem{
			ID:            g.ID.Hex(),
			Name:          g.Name,
			AvatarURL:     g.AvatarURL,
			CreatedBy:     g.CreatedBy.Hex(),
			MemberCount:   len(g.Members),
			MemberInfos:   infos,
			LastMessage:   lastMsg,
			LastMessageAt: lastMsgAt,
			UnreadCount:   unread,
			IsCreator:     g.CreatedBy == user.ID,
		})
	}
	json.NewEncoder(w).Encode(result)
}

func (e *Explorer) apiDMGroupMessages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"not logged in"}`, 401)
		return
	}
	groupIDHex := r.URL.Query().Get("group_id")
	groupID, err := bson.ObjectIDFromHex(groupIDHex)
	if err != nil {
		http.Error(w, `{"error":"invalid group_id"}`, 400)
		return
	}
	group, err := db.GetDMGroup(groupID)
	if err != nil {
		http.Error(w, `{"error":"group not found"}`, 404)
		return
	}
	isMember := false
	for _, m := range group.Members {
		if m == user.ID {
			isMember = true
			break
		}
	}
	if !isMember {
		http.Error(w, `{"error":"not a member"}`, 403)
		return
	}
	msgs, err := db.GetGroupMessages(groupID)
	if err != nil || msgs == nil {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	result := make([]groupMessageJSON, len(msgs))
	for i, m := range msgs {
		result[i] = groupMessageJSON{
			ID:         m.ID.Hex(),
			GroupID:    m.GroupID.Hex(),
			FromID:     m.FromID.Hex(),
			FromPseudo: m.FromPseudo,
			FromAvatar: m.FromAvatar,
			Content:    m.Content,
			GifURL:     m.GifURL,
			ReadBy:     m.ReadBy,
			CreatedAt:  m.CreatedAt,
		}
	}
	json.NewEncoder(w).Encode(result)
}

func (e *Explorer) apiDMGroupSend(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST required"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"not logged in"}`, 401)
		return
	}
	var req struct {
		GroupID string `json:"group_id"`
		Content string `json:"content"`
		GifURL  string `json:"gif_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	groupID, err := bson.ObjectIDFromHex(req.GroupID)
	if err != nil {
		http.Error(w, `{"error":"invalid group_id"}`, 400)
		return
	}
	group, err := db.GetDMGroup(groupID)
	if err != nil {
		http.Error(w, `{"error":"group not found"}`, 404)
		return
	}
	isMember := false
	for _, m := range group.Members {
		if m == user.ID {
			isMember = true
			break
		}
	}
	if !isMember {
		http.Error(w, `{"error":"not a member"}`, 403)
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
	msg := &db.GroupMessage{
		GroupID:    groupID,
		FromID:     user.ID,
		FromPseudo: user.Pseudo,
		FromAvatar: fromAvatar,
		Content:    content,
		GifURL:     req.GifURL,
		ReadBy:     []string{user.ID.Hex()},
	}
	if err := db.SaveGroupMessage(msg); err != nil {
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
	for _, memberID := range group.Members {
		if memberID == user.ID {
			continue
		}
		e.chatHub.SendToUser(memberID.Hex(), map[string]interface{}{
			"type":        "group_message",
			"group_id":    group.ID.Hex(),
			"group_name":  group.Name,
			"from_id":     user.ID.Hex(),
			"from_pseudo": user.Pseudo,
			"from_avatar": fromAvatar,
			"preview":     preview,
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiDMGroupRead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST required"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"not logged in"}`, 401)
		return
	}
	var req struct {
		GroupID string `json:"group_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	groupID, err := bson.ObjectIDFromHex(req.GroupID)
	if err != nil {
		http.Error(w, `{"error":"invalid group_id"}`, 400)
		return
	}
	db.MarkGroupMessagesRead(groupID, user.ID.Hex())
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiDMGroupAddMember(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST required"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"not logged in"}`, 401)
		return
	}
	var req struct {
		GroupID string `json:"group_id"`
		UserID  string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	groupID, err := bson.ObjectIDFromHex(req.GroupID)
	if err != nil {
		http.Error(w, `{"error":"invalid group_id"}`, 400)
		return
	}
	group, err := db.GetDMGroup(groupID)
	if err != nil {
		http.Error(w, `{"error":"group not found"}`, 404)
		return
	}
	if group.CreatedBy != user.ID {
		http.Error(w, `{"error":"creator only"}`, 403)
		return
	}
	if len(group.Members) >= 50 {
		http.Error(w, `{"error":"max 50 members"}`, 400)
		return
	}
	newUID, err := bson.ObjectIDFromHex(req.UserID)
	if err != nil {
		http.Error(w, `{"error":"invalid user_id"}`, 400)
		return
	}
	newUser, err := db.GetUserByID(newUID)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, 404)
		return
	}
	avatar := renderProfilePic(newUser, 32)
	if err := db.AddGroupMember(groupID, newUID, newUser.Pseudo, avatar); err != nil {
		http.Error(w, `{"error":"db error"}`, 500)
		return
	}
	e.chatHub.SendToUser(newUID.Hex(), map[string]interface{}{
		"type":        "group_invite",
		"group_id":    group.ID.Hex(),
		"group_name":  group.Name,
		"from_pseudo": user.Pseudo,
	})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiDMGroupLeave(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST required"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"not logged in"}`, 401)
		return
	}
	var req struct {
		GroupID string `json:"group_id"`
		UserID  string `json:"user_id"` // optional: creator removing another member
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	groupID, err := bson.ObjectIDFromHex(req.GroupID)
	if err != nil {
		http.Error(w, `{"error":"invalid group_id"}`, 400)
		return
	}
	targetID := user.ID
	if req.UserID != "" && req.UserID != user.ID.Hex() {
		group, err := db.GetDMGroup(groupID)
		if err != nil {
			http.Error(w, `{"error":"group not found"}`, 404)
			return
		}
		if group.CreatedBy != user.ID {
			http.Error(w, `{"error":"creator only"}`, 403)
			return
		}
		targetID, err = bson.ObjectIDFromHex(req.UserID)
		if err != nil {
			http.Error(w, `{"error":"invalid user_id"}`, 400)
			return
		}
	}
	if err := db.RemoveGroupMember(groupID, targetID); err != nil {
		http.Error(w, `{"error":"db error"}`, 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (e *Explorer) apiDMGroupRename(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST required"}`, 405)
		return
	}
	user, err := auth.GetSession(r)
	if err != nil || user == nil {
		http.Error(w, `{"error":"not logged in"}`, 401)
		return
	}
	var req struct {
		GroupID string `json:"group_id"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid"}`, 400)
		return
	}
	groupID, err := bson.ObjectIDFromHex(req.GroupID)
	if err != nil {
		http.Error(w, `{"error":"invalid group_id"}`, 400)
		return
	}
	group, err := db.GetDMGroup(groupID)
	if err != nil {
		http.Error(w, `{"error":"group not found"}`, 404)
		return
	}
	if group.CreatedBy != user.ID {
		http.Error(w, `{"error":"creator only"}`, 403)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, `{"error":"name required"}`, 400)
		return
	}
	if err := db.UpdateGroupName(groupID, name); err != nil {
		http.Error(w, `{"error":"db error"}`, 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
