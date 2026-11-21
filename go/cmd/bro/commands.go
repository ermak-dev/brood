// Вход и выход: единственные команды, которым называют сервер и его CA.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"brood.dev/go/brood/clientapi"
)

func (c command) auth(ctx context.Context, name string, args []string) error {
	server, caPath, _, err := parseServer(args)
	if err != nil {
		return err
	}
	client, store, err := newClient(server, caPath)
	if err != nil {
		return err
	}
	switch name {
	case "login":
		return c.login(ctx, client, store)
	case "logout":
		if client.Stored().Session == "" {
			fmt.Fprintln(c.out, "not logged in")
			return nil
		}
		if err := client.Logout(ctx); err != nil {
			return err
		}
		if err := store.Delete(client.Origin()); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "logged out")
	}
	return nil
}

func (c command) login(ctx context.Context, client *clientapi.Client, store *clientapi.SessionStore) error {
	username, err := readLine(c.in, "username: ", c.out)
	if err != nil {
		return err
	}
	fmt.Fprint(c.out, "password: ")
	password, err := readPasswordInput(c.in, c.out)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.out)
	info, err := client.Login(ctx, strings.TrimSpace(username), password)
	if err != nil {
		return err
	}
	if !info.Authenticated || info.User == nil {
		return errors.New("server did not confirm authentication")
	}
	if err := store.Save(client.Origin(), client.Stored()); err != nil {
		return err
	}
	if err := store.SetCurrent(client.Origin()); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "logged in as %s (%s)\n", info.User.Username, client.Origin())
	return nil
}
