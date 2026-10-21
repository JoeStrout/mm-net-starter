// Minimal networked client for the Mini Micro networking starter.
// Click to move; type and press Return to chat; Escape to quit.

import "json"

SERVER = "http://localhost:8737"   // change to your server's address
GAME = "test"
POLL_INTERVAL = 1     // seconds between routine syncs
MIN_SEND_GAP = 0.2    // send pending actions early, but no more often than this
CHAT_LINES = 8
MAX_FRAME_STEP = 1/30 // display time never advances more than this per frame
CATCHUP_RATE = 2     // how quickly display time catches up after a hitch (per sec)
SMOOTH_HALFLIFE = 0.2 // seconds to ease out half of a position correction
SNAP_DIST = 300       // corrections bigger than this snap instead of easing
colors = [color.red, color.aqua, color.lime, color.yellow, color.fuchsia, color.orange]

clear
text.clear
gfx.clear color.clear

// --- clock sync and networking ---

clockOffset = 0       // serverTime is approximately time + clockOffset
bestRtt = 999
lastRtt = 0
lastError = ""        // most recent failure message, so repeats aren't printed
lastStatus = 0        // HTTP status code of the most recent failure, if known

serverNow = function
	return time + clockOffset
end function

// displayTime is the server time we draw at. It follows serverNow, but
// smoothly: after a blocking sync (or a clock-estimate change), things
// briefly speed up to catch up rather than jumping.
displayTime = 0
lastFrame = time

// Advance displayTime for a new frame; returns the step taken.
advanceDisplayTime = function
	step = time - lastFrame
	globals.lastFrame = time
	if step > MAX_FRAME_STEP then step = MAX_FRAME_STEP
	globals.displayTime = displayTime + step
	lag = serverNow - displayTime
	if abs(lag) > 0.5 then
		globals.displayTime = serverNow    // too far off to ease; just jump
	else
		globals.displayTime += lag * CATCHUP_RATE * step
	end if
	return step
end function

// Record a failed post, printing the message only if it's new; returns null.
fail = function(msg, status=0)
	if msg != lastError then print msg
	globals.lastError = msg
	globals.lastStatus = status
	return null
end function

// POST a map as JSON; return the parsed reply map, or null on failure.
post = function(path, data)
	t0 = time
	resp = http.post(SERVER + path, json.toJSON(data), {"Content-Type": "application/json"})
	t1 = time
	globals.lastRtt = t1 - t0
	if resp isa string and resp.indexOf("HTTP/") == 0 then
		// For an error reply, Mini Micro returns only the status line,
		// e.g. "HTTP/1.1 410 Gone".
		return fail(path + ": " + resp, resp.split(" ")[1].val)
	end if
	if not resp or resp[0] != "{" then return fail("Bad response from " + path + ": " + resp)
	result = json.parse(resp)
	if result.hasIndex("error") then return fail("Server error: " + result.error)
	globals.lastError = ""
	// keep the clock estimate from the fastest round trip seen so far
	if t1 - t0 < bestRtt then
		globals.bestRtt = t1 - t0
		globals.clockOffset = result.serverTime - (t0 + t1) / 2
	end if
	return result
end function

// --- players ---

speed = 200           // replaced by the server's value on join
players = {}          // key: player ID; value: Player

Player = {}
Player.name = ""
Player.fx = 0; Player.fy = 0   // position at time t0
Player.tx = 0; Player.ty = 0   // destination
Player.t0 = 0                  // server time when this motion began
Player.ox = 0; Player.oy = 0   // drawing offset, eased toward 0 after a correction

Player.make = function(id, name, shape)
	p = new Player
	p.id = id
	p.name = name
	p.sprite = new Sprite
	p.sprite.image = file.loadImage("/sys/pics/shapes/" + shape + ".png")
	p.sprite.scale = 0.5
	p.sprite.tint = colors[(id - 1) % colors.len]
	display(4).sprites.push p.sprite
	return p
end function

Player.removeSprite = function
	sprites = display(4).sprites
	i = sprites.indexOf(self.sprite)
	if i != null then sprites.remove i
end function

// Set a new motion. If smooth is true, set the drawing offset so the player
// is drawn where it already was, and ease from there (see draw).
Player.setMotion = function(fx, fy, tx, ty, t0, smooth=true)
	wasAt = self.displayPos
	self.fx = fx; self.fy = fy
	self.tx = tx; self.ty = ty
	self.t0 = t0
	nowAt = self.posAt(displayTime)
	self.ox = 0; self.oy = 0
	if not smooth then return
	dx = wasAt[0] - nowAt[0]
	dy = wasAt[1] - nowAt[1]
	if dx^2 + dy^2 > SNAP_DIST^2 then return
	self.ox = dx; self.oy = dy
end function

Player.displayPos = function
	pos = self.posAt(displayTime)
	return [pos[0] + self.ox, pos[1] + self.oy]
end function

Player.posAt = function(t)
	dx = self.tx - self.fx
	dy = self.ty - self.fy
	d = sqrt(dx^2 + dy^2)
	if d == 0 then return [self.tx, self.ty]
	f = speed * (t - self.t0) / d
	if f > 1 then f = 1
	if f < 0 then f = 0
	return [self.fx + dx * f, self.fy + dy * f]
end function

// --- chat ---

chatLog = []
chatInput = ""

addChat = function(line)
	chatLog.push line
	if chatLog.len > CHAT_LINES then chatLog.pull
end function

// --- event handling ---

handleEvent = function(ev)
	if ev.type == "join" then
		if not players.hasIndex(ev.from) then players[ev.from] = Player.make(ev.from, ev.name, ev.shape)
		players[ev.from].setMotion ev.fx, ev.fy, ev.x, ev.y, ev.t, false
		addChat "* " + ev.name + " joined"
	else if ev.type == "move" then
		// The server's version replaces our own local prediction, too.
		players[ev.from].setMotion ev.fx, ev.fy, ev.x, ev.y, ev.t
	else if ev.type == "chat" then
		addChat ev.name + ": " + ev.text
	else if ev.type == "leave" then
		if players.hasIndex(ev.from) then
			p = players[ev.from]
			p.removeSprite
			players.remove ev.from
			addChat "* " + p.name + " left"
		end if
	end if
end function

outbox = []           // actions not yet sent to the server
lastSeq = 0
lastSync = -999
connected = true

sync = function
	data = {"player": myId, "token": myToken, "after": lastSeq, "events": outbox}
	globals.lastSync = time
	result = post("/games/" + GAME + "/sync", data)
	if result == null then
		// 410: the server dropped us for not syncing recently.
		// 404: the whole game is gone.
		if lastStatus == 410 or lastStatus == 404 then globals.connected = false
		return    // otherwise keep outbox and retry next time
	end if
	globals.outbox = []
	for ev in result.events
		handleEvent ev
	end for
	globals.lastSeq = result.last
end function

// --- join ---

myName = input("Your name: ")
reply = post("/games/" + GAME + "/join", {"name": myName})
if reply == null then exit
myId = reply.player
myToken = reply.token
speed = reply.speed
// The join reply has a snapshot of the game, so we sync only from our join on.
for info in reply.players
	players[info.id] = Player.make(info.id, info.name, info.shape)
	players[info.id].setMotion info.fx, info.fy, info.x, info.y, info.t, false
end for
for ev in reply.recentChat
	handleEvent ev
end for
addChat "* You joined as " + myName
lastSeq = reply.last
displayTime = serverNow
text.clear
sync

// --- input ---

wasDown = false

handleMouse = function
	down = mouse.button
	if down and not wasDown and players.hasIndex(myId) then
		// Predict our own move immediately; the server's echo corrects it.
		// We send the click time so the server's version starts when ours did.
		me = players[myId]
		t = serverNow
		pos = me.posAt(t)
		me.setMotion pos[0], pos[1], mouse.x, mouse.y, t
		outbox.push {"type": "move", "x": mouse.x, "y": mouse.y, "t": t}
	end if
	globals.wasDown = down
end function

// Returns false when the user wants to quit; true to continue.
handleKeys = function
	while key.available
		k = key.get
		if k == char(27) then return false
		if k == char(10) or k == char(13) then
			if chatInput then outbox.push {"type": "chat", "text": chatInput}
			globals.chatInput = ""
		else if k == char(8) then
			globals.chatInput = chatInput[:-1]
		else if k.code >= 32 then
			globals.chatInput = chatInput + k
		end if
	end while
	return true
end function

// --- drawing ---

draw = function
	// Note: this is rather expensive, redrawing the whole PixelDisplay
	// on every frame.  Much better would be to make the player names part
	// of the player sprites, and only redraw the chat and text input when
	// needed.  But hey, it's only a demo.  :) 
	gfx.clear color.clear
	step = advanceDisplayTime
	decay = 0.5 ^ (step / SMOOTH_HALFLIFE)
	for p in players.values
		p.ox = p.ox * decay
		p.oy = p.oy * decay
		pos = p.displayPos
		p.sprite.x = pos[0]
		p.sprite.y = pos[1]
		gfx.print p.name, pos[0] - p.name.len * 4, pos[1] + 24, color.white, "small"
	end for
	y = 620
	for line in chatLog
		gfx.print line, 8, y, color.silver, "small"
		y -= 16
	end for
	gfx.print "> " + chatInput + "_", 8, 8, color.white, "small"
	gfx.print "rtt " + round(lastRtt * 1000) + " ms", 860, 8, color.gray, "small"
end function

// --- main loop ---

while connected and handleKeys
	handleMouse
	draw
	yield    // show this frame before sync (possibly) blocks
	if time - lastSync >= POLL_INTERVAL or (outbox and time - lastSync >= MIN_SEND_GAP) then sync
end while
if not connected then print "Disconnected from the game."
