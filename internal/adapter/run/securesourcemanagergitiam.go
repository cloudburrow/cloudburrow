package run

import (
	"net/http"
	"strings"
)

// SourceManagerGitRequest is what a git request to a repository's URL
// asks for (#1879, round 12): the repository's name and its instance's,
// whether it is part of a push, and the caller's access token (a bearer
// token, or the password of basic credentials, as git's credential
// helpers send it). ok is false for another path or a repository that
// does not exist; link is true for a Developer Connect token, which its
// link's scope checks, not IAM.
func SourceManagerGitRequest(r *http.Request) (repo, instance string, push bool, token string, link, ok bool) {
	if !IsSourceManagerGitPath(r.URL.Path) {
		return "", "", false, "", false, false
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, ssmGitPrefix), "/", 4)
	if len(parts) < 3 || !strings.HasSuffix(parts[2], ".git") {
		return "", "", false, "", false, false
	}
	rp, _, found := lookupSSMRepo(parts[0], parts[1], strings.TrimSuffix(parts[2], ".git"))
	if !found {
		return "", "", false, "", false, false
	}
	token = gitRequestToken(r)
	_, link = linkTokenAccount(token)
	return rp.GetName(), rp.GetInstance(), isGitWrite(r), token, link, true
}
