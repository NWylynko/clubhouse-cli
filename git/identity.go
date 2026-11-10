package git

import "fmt"

type Identity struct {
	Name  string
	Email string
}

var HermeticIdentity = Identity{Name: "club", Email: "club@club.invalid"}

func SessionIdentity(name, pin string) Identity {
	return Identity{
		Name:  name,
		Email: fmt.Sprintf("%s@club-%s.local", name, pin),
	}
}

func (id Identity) EnvVars() []string {
	return []string{
		"GIT_AUTHOR_NAME=" + id.Name,
		"GIT_AUTHOR_EMAIL=" + id.Email,
		"GIT_COMMITTER_NAME=" + id.Name,
		"GIT_COMMITTER_EMAIL=" + id.Email,
	}
}
