#!/bin/sh
set -eu

VERSION=${VERSION:-0.0.0}
DMG="dist/ClusBot-${VERSION}-arm64.dmg"

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
app="$stage/ClusBot.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" dist
cp macos/Info.plist "$app/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $VERSION" -c "Set :CFBundleVersion $VERSION" "$app/Contents/Info.plist"
swift macos/generate-icon.swift web/src/clusbot-brand.svg "$stage/ClusBot.iconset"
iconutil -c icns "$stage/ClusBot.iconset" -o "$app/Contents/Resources/ClusBot.icns"
rm -rf "$stage/ClusBot.iconset"
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$app/Contents/MacOS/clusbot-service" ./cmd/clusbot
xcrun swiftc -O -target arm64-apple-macos14.0 -o "$app/Contents/MacOS/ClusBot" macos/main.swift
codesign --force --sign - "$app/Contents/MacOS/clusbot-service"
codesign --force --sign - "$app"
codesign --verify --deep --strict "$app"
ln -s /Applications "$stage/Applications"
hdiutil create -ov -volname ClusBot -srcfolder "$stage" -format UDZO "$DMG"
