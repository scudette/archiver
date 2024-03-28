all:
	go build -o go_zip -ldflags="-s -w -linkmode external -extldflags \"-static\""
