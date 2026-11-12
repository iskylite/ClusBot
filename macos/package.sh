#!/bin/sh
set -eu

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
app="$stage/ClusBot.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" dist
cp macos/Info.plist "$app/Contents/Info.plist"
cp macos/ClusBot.icns "$app/Contents/Resources/"
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$app/Contents/MacOS/clusbot-service" ./cmd/clusbot
xcrun swiftc -O -target arm64-apple-macos14.0 -o "$app/Contents/MacOS/ClusBot" macos/main.swift
codesign --force --sign - "$app/Contents/MacOS/clusbot-service"
codesign --force --sign - "$app"
codesign --verify --deep --strict "$app"
ln -s /Applications "$stage/Applications"
hdiutil create -ov -volname ClusBot -srcfolder "$stage" -format UDZO dist/ClusBot-arm64.dmg
