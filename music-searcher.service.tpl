[Unit]
Description=Music Searcher Bot
Documentation=https://github.com/WarrDoge/telegram-music-searcher-bot
After=network.target
Wants=network.target

[Service]
Type=simple
User=root
Group=root
WorkingDirectory=/root
ExecStart=/root/CHANGE_ME_1
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5
TimeoutStopSec=30

Environment=TELEGRAM_BOT_TOKEN='CHANGE_ME_2'
Environment=BOT_DEBUG=0

[Install]
WantedBy=multi-user.target
