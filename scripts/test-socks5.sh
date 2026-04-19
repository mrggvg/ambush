#!/usr/bin/env bash
SOCKS5_USER=${SOCKS5_USER:-scraper1}
SOCKS5_PASS=${SOCKS5_PASS:-supersecret}
SOCKS5_ADDR=${SOCKS5_ADDR:-localhost:1080}

curl -x "socks5://${SOCKS5_USER}:${SOCKS5_PASS}@${SOCKS5_ADDR}" https://example.com
