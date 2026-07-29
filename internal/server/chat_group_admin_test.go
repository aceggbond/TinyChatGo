package server

import (
	"io"
	"testing"
	"time"
)

func TestAdministratorManagesUserCreatedGroup(t *testing.T) {
	s := New(io.Discard)
	now := time.Now().UTC()
	s.chat.mu.Lock()
	s.chat.userGroups["g-admin"] = &chatUserGroupState{ChatGroup: ChatGroup{
		ID:        "g-admin",
		Name:      "原群名",
		OwnerIP:   "192.0.2.10",
		Members:   []string{"192.0.2.10", "192.0.2.11"},
		CreatedAt: now,
		UpdatedAt: now,
	}}
	s.chat.mu.Unlock()

	if err := s.RenameChatGroup("g-admin", "新群名"); err != nil {
		t.Fatal(err)
	}
	groups := s.ChatGroups()
	if len(groups) != 1 || groups[0].Name != "新群名" {
		t.Fatalf("renamed group = %#v", groups)
	}
	if err := s.RemoveChatGroupMember("g-admin", "192.0.2.10"); err == nil {
		t.Fatal("group owner was removable")
	}
	if err := s.RemoveChatGroupMember("g-admin", "192.0.2.11"); err != nil {
		t.Fatal(err)
	}
	groups = s.ChatGroups()
	if len(groups) != 1 || len(groups[0].Members) != 1 || groups[0].Members[0] != "192.0.2.10" {
		t.Fatalf("group after member removal = %#v", groups)
	}
	if err := s.DeleteChatGroup("g-admin"); err != nil {
		t.Fatal(err)
	}
	if groups = s.ChatGroups(); len(groups) != 0 {
		t.Fatalf("deleted group remains: %#v", groups)
	}
}

func TestPublicGroupIncludesMembersForMentionPicker(t *testing.T) {
	hub := newChatHub()
	hub.userGroups["g-mention"] = &chatUserGroupState{ChatGroup: ChatGroup{
		ID:      "g-mention",
		Name:    "项目群",
		OwnerIP: "192.0.2.10",
		Members: []string{"192.0.2.10", "192.0.2.11"},
	}}
	groups := hub.publicGroupsLocked("192.0.2.10")
	if len(groups) != 1 || len(groups[0].Members) != 2 ||
		groups[0].Members[0] != "192.0.2.10" || groups[0].Members[1] != "192.0.2.11" {
		t.Fatalf("public group members = %#v", groups)
	}
	groups[0].Members[0] = "changed"
	if hub.userGroups["g-mention"].Members[0] != "192.0.2.10" {
		t.Fatal("public group exposed the mutable member slice")
	}
}

func TestGroupOwnerCanRenameInviteAndRemoveMembers(t *testing.T) {
	hub := newChatHub()
	owner := &chatPeer{ip: "192.0.2.10"}
	nonOwner := &chatPeer{ip: "192.0.2.11"}
	for _, ip := range []string{"192.0.2.10", "192.0.2.11", "192.0.2.12"} {
		hub.users[ip] = &ChatUser{IP: ip}
	}
	hub.userGroups["g-owner"] = &chatUserGroupState{ChatGroup: ChatGroup{
		ID:      "g-owner",
		Name:    "原群名",
		OwnerIP: owner.ip,
		Members: []string{owner.ip, nonOwner.ip},
	}}

	if err := hub.renameUserGroup(nonOwner, "g-owner", "无权限"); err == nil {
		t.Fatal("non-owner renamed the group")
	}
	if err := hub.renameUserGroup(owner, "g-owner", "新群名"); err != nil {
		t.Fatal(err)
	}
	if err := hub.addUserGroupMembers(owner, "g-owner", []string{"192.0.2.12"}); err != nil {
		t.Fatal(err)
	}
	if err := hub.removeUserGroupMember(owner, "g-owner", nonOwner.ip); err != nil {
		t.Fatal(err)
	}

	group := hub.userGroups["g-owner"]
	if group.Name != "新群名" {
		t.Fatalf("group name = %q", group.Name)
	}
	if len(group.Members) != 2 || group.Members[0] != owner.ip || group.Members[1] != "192.0.2.12" {
		t.Fatalf("group members = %#v", group.Members)
	}
}
