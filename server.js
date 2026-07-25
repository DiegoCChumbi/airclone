const express = require("express");
const http = require("http");
const socketio = require("socket.io");
const dgram = require("dgram");
const os = require("os");
const qrcode = require("qrcode");

const app = express();
const server = http.createServer(app);
const io = socketio(server);
const udp = dgram.createSocket("udp4");

app.use(express.static("public"));

const players = new Map(); // Stores { socket.id -> {username, controllerId} }
const activeUsernames = new Set(); 

function getLocalIP() {
    const interfaces = os.networkInterfaces();
    for (const name of Object.keys(interfaces)) {
        for (const iface of interfaces[name]) {
            if (iface.family === 'IPv4' && !iface.internal) {
                return iface.address;
            }
        }
    }
    return 'localhost';
}

function sendToParent(message) {
    process.stdout.write(JSON.stringify(message) + '\n');
}

// Accept commands from the Go parent process via stdin (e.g. generate_qr)
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, terminal: false });
rl.on('line', (line) => {
    try {
        const cmd = JSON.parse(line.trim());
        if (cmd.cmd === 'generate_qr' && cmd.url) {
            qrcode.toString(cmd.url, { type: 'terminal', small: true }, (err, qrString) => {
                sendToParent({
                    event: 'server_ready',
                    url: cmd.url,
                    qr: qrString || 'Could not generate QR code.'
                });
            });
        }
    } catch (_) { /* ignore malformed lines */ }
});

io.on("connection", socket => {
    socket.on("register_player", ({ username }) => {
        if (activeUsernames.has(username)) {
            socket.emit('registration_failed', `El nombre "${username}" ya está en uso.`);
            return;
        }

        const msg = Buffer.from(`${username}:get_id`);
        udp.send(msg, 9999, "127.0.0.1", (err) => {
            if(err){
                socket.emit('registration_failed', `Error interno del servidor.`);
                return
            }
        });

        const listener = (data) => {
            const parts = data.toString().split(":");
            if(parts.length < 3 || parts[0] !== username || parts[1] !== 'controller_id'){
                return;
            }

            const controllerId = parseInt(parts[2], 10);

            const playerData = { username, controllerId };
            players.set(socket.id, playerData);
            activeUsernames.add(username);

            sendToParent({
                event: "player_connect",
                username: username,
                controllerId: controllerId
            });

            socket.emit('registration_success');
            udp.removeListener('message', listener);
        }
        udp.on('message', listener)
    });

    socket.on("input", ({ button, state }) => {
        if (!players.has(socket.id)) return;
        const { username } = players.get(socket.id);
        const msg = Buffer.from(`${username}:btn:${button}:${state}`);
        udp.send(msg, 9999, "127.0.0.1");
    });

    socket.on("axis", ({ axis, value }) => {
        if (!players.has(socket.id)) return;
        const { username } = players.get(socket.id);
        const msg = Buffer.from(`${username}:axis:${axis}:${value.toFixed(4)}`);
        udp.send(msg, 9999, "127.0.0.1");
    });

    socket.on("disconnect", () => {
        if (players.has(socket.id)) {
            const { username } = players.get(socket.id);
            const msg = Buffer.from(`${username}:disconnect`);
            udp.send(msg, 9999, "127.0.0.1");

            players.delete(socket.id);
            activeUsernames.delete(username);

            sendToParent({
                event: "player_disconnect",
                username: username
            });
        }
    });
});

const PORT = 3000;
server.listen(PORT, () => {
    const ip = getLocalIP();
    const url = `http://${ip}:${PORT}`;

    qrcode.toString(url, { type: 'terminal', small: true }, (err, qrString) => {
        sendToParent({
            event: "server_ready",
            url: url,
            qr: qrString || "Could not generate QR code."
        });
    });
});
