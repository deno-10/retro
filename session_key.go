package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Создаём ключ рядом с локальной БД один раз. Параллельный запуск видит
// полностью записанный ключ победителя. Повреждённый ключ не заменяем.
func loadSessionKey(path string) (key [32]byte, err error) {
	read := func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(data) != len(key) {
			return fmt.Errorf("неверный размер ключа сессий в %s", path)
		}
		copy(key[:], data)
		return nil
	}
	if err = read(); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return key, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".retro-session-*")
	if err != nil {
		return key, err
	}
	defer os.Remove(f.Name())
	rand.Read(key[:])
	_, err = f.Write(key[:])
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return key, err
	}
	if err = os.Link(f.Name(), path); errors.Is(err, os.ErrExist) {
		err = read()
	}
	return
}
