package main

import "testing"

func TestCoversGitHubInvitationsRequiresNarrowInvitationTerms(t *testing.T) {
	if coversGitHubInvitations([]string{"from:example.edu", "deadline OR smartlms"}) {
		t.Fatal("unrelated Gmail queries claimed to cover GitHub invitations")
	}
	if coversGitHubInvitations([]string{"from:github.com newer_than:30d"}) {
		t.Fatal("all GitHub mail claimed to be a narrow invitation query")
	}
	if !coversGitHubInvitations([]string{"from:(github.com) (invitation OR invited) newer_than:30d"}) {
		t.Fatal("GitHub invitation query was not recognized")
	}
}
