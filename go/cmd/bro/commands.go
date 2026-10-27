// Команды bro: вход, выход, сведения о пользователе и список проектов.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"brood.dev/go/brood/clientapi"
)

func cmdLogin(ctx context.Context, args []string) int {
	server, caPath, _, err := parseServer(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bro login:", err)
		return 1
	}
	client, store, err := newClient(server, caPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bro login:", err)
		return 1
	}
	fmt.Print("username: ")
	username, err := readLine(os.Stdin, "", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bro login:", err)
		return 1
	}
	fmt.Print("password: ")
	password, err := readPasswordInput(os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bro login:", err)
		return 1
	}
	fmt.Println()
	info, err := client.Login(ctx, strings.TrimSpace(username), password)
	if err != nil {
		if errors.Is(err, clientapi.ErrInvalidCredentials) {
			fmt.Fprintln(os.Stderr, "bro login: invalid credentials")
			return 2
		}
		fmt.Fprintln(os.Stderr, "bro login:", err)
		return 1
	}
	if err := store.Save(client.Origin(), client.Stored()); err != nil {
		fmt.Fprintln(os.Stderr, "bro login:", err)
		return 1
	}
	if err := store.SetCurrent(client.Origin()); err != nil {
		fmt.Fprintln(os.Stderr, "bro login:", err)
		return 1
	}
	fmt.Printf("logged in as %s (%s)\n", info.User.Username, client.Origin())
	return 0
}

func cmdLogout(ctx context.Context, args []string) int {
	server, caPath, _, err := parseServer(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bro logout:", err)
		return 1
	}
	client, store, err := newClient(server, caPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bro logout:", err)
		return 1
	}
	if client.Stored().Empty() {
		fmt.Println("not logged in")
		return 0
	}
	// Сетевой отказ не выдаётся за успешный серверный отзыв: локальная запись
	// остаётся, но и успех не объявляется.
	if err := client.Logout(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "bro logout:", err)
		if delErr := store.Delete(client.Origin()); delErr != nil {
			fmt.Fprintln(os.Stderr, "bro logout:", delErr)
		}
		return 1
	}
	if err := store.Delete(client.Origin()); err != nil {
		fmt.Fprintln(os.Stderr, "bro logout:", err)
		return 1
	}
	fmt.Println("logged out")
	return 0
}

func cmdWhoami(ctx context.Context, args []string) int {
	client, _, err := newClient(mustServer(args), "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "bro whoami:", err)
		return 1
	}
	user, err := client.Whoami(ctx)
	if err != nil {
		return authFailure("bro whoami", err)
	}
	fmt.Printf("%s", user.Username)
	if user.DisplayName != "" {
		fmt.Printf(" (%s)", user.DisplayName)
	}
	if user.IsAdmin {
		fmt.Print(" [admin]")
	}
	fmt.Println()
	return 0
}

func cmdProjects(ctx context.Context, args []string) int {
	rest := args
	if len(rest) > 0 && rest[0] == "list" {
		rest = rest[1:]
	}
	client, _, err := newClient(mustServer(rest), "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "bro projects:", err)
		return 1
	}
	projects, err := client.ProjectsList(ctx)
	if err != nil {
		return authFailure("bro projects", err)
	}
	if len(projects) == 0 {
		fmt.Println("no projects")
		return 0
	}
	for _, project := range projects {
		fmt.Println(project.ID, project.Name)
	}
	return 0
}

func mustServer(args []string) string {
	server, _, _, err := parseServer(args)
	if err != nil {
		return ""
	}
	return server
}

func authFailure(command string, err error) int {
	if errors.Is(err, clientapi.ErrSessionExpired) {
		fmt.Fprintln(os.Stderr, command+": session expired; run bro login <server>")
		return 2
	}
	fmt.Fprintln(os.Stderr, command+":", err)
	return 1
}
