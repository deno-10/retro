package main

import (
	"net"
	"reflect"
	"testing"
)

func TestShareURLs(t *testing.T) {
	ips := []net.IP{net.ParseIP("192.168.1.5").To4(), net.ParseIP("10.0.0.7").To4()}

	got := shareURLs(":8080", ips)
	want := []string{"http://localhost:8080", "http://192.168.1.5:8080", "http://10.0.0.7:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("все интерфейсы: ожидалось %v, получено %v", want, got)
	}

	got = shareURLs("0.0.0.0:9000", nil)
	if want := []string{"http://localhost:9000"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("без адресов в сети остаётся только localhost: %v", got)
	}

	got = shareURLs("127.0.0.1:8080", ips)
	if want := []string{"http://127.0.0.1:8080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("конкретный адрес не подменяется: %v", got)
	}

	if got := shareURLs("нет порта", ips); got != nil {
		t.Fatalf("некорректный адрес: ожидался nil, получено %v", got)
	}
}
