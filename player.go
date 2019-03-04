package mcwss

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/sandertv/mcwss/mctype"
	"github.com/sandertv/mcwss/protocol"
	"github.com/sandertv/mcwss/protocol/command"
	"github.com/sandertv/mcwss/protocol/event"
	"log"
	"reflect"
	"sync"
)

// Player is a player connected to the websocket server.
type Player struct {
	*websocket.Conn

	name      string
	connected bool
	event.Properties

	agent *Agent
	world *World

	sync.Mutex
	handlers         map[event.Name]func(event interface{})
	commandCallbacks map[string]reflect.Value

	close       chan bool
	packetStack chan interface{}

	lastReceivedMessage string
}

// NewPlayer returns an initialised player for a websocket connection.
func NewPlayer(conn *websocket.Conn) *Player {
	player := &Player{
		Conn:             conn,
		connected:        true,
		handlers:         make(map[event.Name]func(event interface{})),
		commandCallbacks: make(map[string]reflect.Value),
		packetStack:      make(chan interface{}, 64),
		close:            make(chan bool, 1),
	}
	player.Exec(command.LocalPlayerNameRequest(), func(response *command.LocalPlayerName) {
		player.name = response.LocalPlayerName
	})
	player.agent = NewAgent(player)
	player.world = NewWorld(player)
	go player.sendPackets()
	return player
}

// Name returns the name of the player.
func (player *Player) Name() string {
	return player.name
}

// Agent returns the controllable agent entity of the player.
func (player *Player) Agent() *Agent {
	return player.agent
}

// World returns the world of the player.
func (player *Player) World() *World {
	return player.world
}

// SendMessage sends a message that only the player receives. Its behaviour is synonymous with fmt.Sprintf(),
// putting all parameters in the string using formatting identifiers.
func (player *Player) SendMessage(message string, parameters ...interface{}) {
	message = fmt.Sprintf(escapeMessage(message), parameters...)
	player.Exec(command.TellRawRequest(mctype.Target(player.name), message), nil)
}

// Tell tells the player a private message. Its behaviour is synonymous with fmt.Sprintf(), putting all
// parameters in the string using formatting identifiers.
func (player *Player) Tell(message string, parameters ...interface{}) {
	message = fmt.Sprintf(message, parameters)
	player.Exec(command.TellRequest(mctype.Target(player.name), message), nil)
}

// Say broadcasts a message as the player to all players in the world of the player.
func (player *Player) Say(message string, parameters ...interface{}) {
	message = fmt.Sprintf(message, parameters...)
	player.ExecAs(command.SayRequest(message), nil)
}

// Position requests the position of a player and calls the function passed when a response is received,
// containing the position of the player.
func (player *Player) Position(f func(position mctype.Position)) {
	player.Exec(command.QueryTargetRequest(mctype.Target(player.name)), func(response *command.QueryTarget) {
		if len(*response.Details) == 1 {
			f((*response.Details)[0].Position)
		}
	})
}

// Connected checks if a player is currently connected. If not, the reference to this player should be
// released as soon as possible.
func (player *Player) Connected() bool {
	return player.connected
}

// Exec sends a command string with a callback that can process the output of the command. The callback passed
// must have one input argument, being the container of the output.
// This may be done using either a pointer to a struct, or a map, like so:
//
// player.Exec("getlocalplayername", func(response *command.LocalPlayerName){})
// player.Exec("getlocalplayername", func(response map[string]interface{}){})
//
// Nil may also be passed if no callback needs to be executed.
func (player *Player) Exec(commandLine string, callback interface{}) {
	val := reflect.ValueOf(callback)
	if callback != nil {
		t := val.Type()
		// Do some basic function validation.
		if t.Kind() != reflect.Func || t.NumIn() != 1 || (t.In(0).Kind() != reflect.Ptr && t.In(0).Kind() != reflect.Map) {
			panic("invalid callback type passed. must be of type func(*commandResponse)")
		}
	}
	packet := protocol.NewCommandRequest(commandLine)
	player.Lock()
	player.commandCallbacks[packet.Header.RequestID] = val
	player.Unlock()
	_ = player.WriteJSON(packet)
}

// ExecAs sends a command string as if it were sent by the player itself with a callback that can process the
// output of the command. The callback passed must have one input argument, being the container of the output.
// The output of the command is captured by the player, not by the websocket server. Only a status code is
// captured by the server that indicates if the command was successful.
func (player *Player) ExecAs(commandLine string, callback func(statusCode int)) {
	player.Exec(fmt.Sprintf("execute %v ~ ~ ~ %v", player.name, commandLine), func(response map[string]interface{}) {
		codeInterface, exists := response["statusCode"]
		if !exists {
			log.Printf("exec as: invalid response JSON")
			return
		}
		code, _ := codeInterface.(int)
		if callback != nil {
			callback(code)
		}
	})
}

// OnMobKilled listens for entities that were killed by the entity. Note that indirect killing methods such as
// drowning an entity, hitting off an edge, suffocating it etc. does not trigger the event.
func (player *Player) OnMobKilled(handler func(event *event.MobKilled)) {
	player.on(event.NameMobKilled, func(e interface{}) {
		handler(e.(*event.MobKilled))
	})
}

// OnAwardAchievement listens to achievements awarded to the player. Note that achievements are not awarded in
// worlds that have cheats enabled.
func (player *Player) OnAwardAchievement(handler func(event *event.AwardAchievement)) {
	player.on(event.NameAwardAchievement, func(e interface{}) {
		handler(e.(*event.AwardAchievement))
	})
}

// OnSlashCommandExecuted listens for commands executed by a player that actually existed. Unknown commands do
// not result in this event being called.
func (player *Player) OnSlashCommandExecuted(handler func(event *event.SlashCommandExecuted)) {
	player.on(event.NameSlashCommandExecuted, func(e interface{}) {
		handler(e.(*event.SlashCommandExecuted))
	})
}

// OnScreenChanged listens for the screen of the player being changed. Note that this is not sent every time
// anything is changed on the screen, but rather when a player switches to a completely different screen.
func (player *Player) OnScreenChanged(handler func(event *event.ScreenChanged)) {
	player.on(event.NameScreenChanged, func(e interface{}) {
		handler(e.(*event.ScreenChanged))
	})
}

// OnScriptBroadcastEvent listens for scripts ran by the player that broadcast events. Due to the nature of
// this event, scripts could send JSON objects as event to listen on to communicate with the websocket server
// for interaction between the two.
func (player *Player) OnScriptBroadcastEvent(handler func(event *event.ScriptBroadcastEvent)) {
	player.on(event.NameScriptBroadcastEvent, func(e interface{}) {
		handler(e.(*event.ScriptBroadcastEvent))
	})
}

// OnScriptError listens for 'non-critical' errors encountered in scripts ran by the player. These errors
// generally have a very detailed error message.
func (player *Player) OnScriptError(handler func(event *event.ScriptError)) {
	player.on(event.NameScriptError, func(e interface{}) {
		handler(e.(*event.ScriptError))
	})
}

// OnScriptGetComponent listens for scripts calling the getComponent method.
func (player *Player) OnScriptGetComponent(handler func(event *event.ScriptGetComponent)) {
	player.on(event.NameScriptGetComponent, func(e interface{}) {
		handler(e.(*event.ScriptGetComponent))
	})
}

// OnScriptInternalError listens for 'critical' errors encountered during the initial loading of a script.
// These errors often have a very vague error message, often pointing back to a syntax error or other critical
// error found in the script.
func (player *Player) OnScriptInternalError(handler func(event *event.ScriptInternalError)) {
	player.on(event.NameScriptInternalError, func(e interface{}) {
		handler(e.(*event.ScriptInternalError))
	})
}

// OnScriptListenToEvent listens for scripts ran by the client that start listening for events.
func (player *Player) OnScriptListenToEvent(handler func(event *event.ScriptListenToEvent)) {
	player.on(event.NameScriptListenToEvent, func(e interface{}) {
		handler(e.(*event.ScriptListenToEvent))
	})
}

// OnScriptLoaded listens for scripts that are loaded by the player.
func (player *Player) OnScriptLoaded(handler func(event *event.ScriptLoaded)) {
	player.on(event.NameScriptLoaded, func(e interface{}) {
		handler(e.(*event.ScriptLoaded))
	})
}

// OnScriptRan listens for scripts that are ran immediately after they are loaded. This event is called once
// immediately hen the player spawns, even though the script might still be running after that.
func (player *Player) OnScriptRan(handler func(event *event.ScriptRan)) {
	player.on(event.NameScriptRan, func(e interface{}) {
		handler(e.(*event.ScriptRan))
	})
}

// OnStartWorld subscribes to events called when the player starts a world by clicking it in the main menu.
// It includes both servers and singleplayer worlds. The event provides no information about the world.
func (player *Player) OnStartWorld(handler func(event *event.StartWorld)) {
	player.on(event.NameStartWorld, func(e interface{}) {
		handler(e.(*event.StartWorld))
	})
}

// OnWorldLoaded subscribes to events called when the player loads a world. This happens both when joining
// servers and singleplayer worlds, and happens directly after the StartGamePacket is called. The event
// supplies some of the data of this packet.
func (player *Player) OnWorldLoaded(handler func(event *event.WorldLoaded)) {
	player.on(event.NameWorldLoaded, func(e interface{}) {
		handler(e.(*event.WorldLoaded))
	})
}

// OnWorldGenerated subscribes to events called when a player generates a new singleplayer world.
func (player *Player) OnWorldGenerated(handler func(event *event.WorldGenerated)) {
	player.on(event.NameWorldGenerated, func(e interface{}) {
		handler(e.(*event.WorldGenerated))
	})
}

// OnMobInteracted subscribes to events called when the player interacts with a mob, in a way that has
// actually has a result.
func (player *Player) OnMobInteracted(handler func(event *event.MobInteracted)) {
	player.on(event.NameMobInteracted, func(e interface{}) {
		handler(e.(*event.MobInteracted))
	})
}

// OnEndOfDay subscribes to events called when the end of a day was reached naturally. (without commands)
func (player *Player) OnEndOfDay(handler func(event *event.EndOfDay)) {
	player.on(event.NameEndOfDay, func(e interface{}) {
		handler(e.(*event.EndOfDay))
	})
}

// OnSignedBookOpened subscribes to signed books opened by the player.
func (player *Player) OnSignedBookOpened(handler func(event *event.SignedBookOpened)) {
	player.on(event.NameSignedBookOpened, func(e interface{}) {
		handler(e.(*event.SignedBookOpened))
	})
}

// OnBookEdited subscribes to edits by the player to a book after closing it.
func (player *Player) OnBookEdited(handler func(event *event.BookEdited)) {
	player.on(event.NameBookEdited, func(e interface{}) {
		handler(e.(*event.BookEdited))
	})
}

// OnTransform subscribes to transformations done to the player, usually sent by means such as teleporting.
func (player *Player) OnTransform(handler func(event *event.PlayerTransform)) {
	player.on(event.NamePlayerTransform, func(e interface{}) {
		handler(e.(*event.PlayerTransform))
	})
}

// OnTravelled subscribes to the player travelling to places.
func (player *Player) OnTravelled(handler func(event *event.PlayerTravelled)) {
	player.on(event.NamePlayerTravelled, func(e interface{}) {
		handler(e.(*event.PlayerTravelled))
	})
}

// OnItemAcquired listens to items acquired by the player, for example by picking items up or by getting items
// from a chest.
func (player *Player) OnItemAcquired(handler func(event *event.ItemAcquired)) {
	player.on(event.NameItemAcquired, func(e interface{}) {
		handler(e.(*event.ItemAcquired))
	})
}

// OnItemDropped listens for items dropped on the ground by the player.
func (player *Player) OnItemDropped(handler func(event *event.ItemDropped)) {
	player.on(event.NameItemDropped, func(e interface{}) {
		handler(e.(*event.ItemDropped))
	})
}

// OnItemUsed subscribes to items used by the player.
func (player *Player) OnItemUsed(handler func(event *event.ItemUsed)) {
	player.on(event.NameItemUsed, func(e interface{}) {
		handler(e.(*event.ItemUsed))
	})
}

// OnItemInteracted subscribes to interactions made using items by the player.
func (player *Player) OnItemInteracted(handler func(event *event.ItemInteracted)) {
	player.on(event.NameItemInteracted, func(e interface{}) {
		handler(e.(*event.ItemInteracted))
	})
}

// OnItemEquipped listens for items equipped by the player. This includes armour, pumpkins, elytras and other
// items that may be worn.
func (player *Player) OnItemEquipped(handler func(event *event.ItemEquipped)) {
	player.on(event.NameItemEquipped, func(e interface{}) {
		handler(e.(*event.ItemEquipped))
	})
}

// OnItemCrafted subscribes to items crafted by the player.
func (player *Player) OnItemCrafted(handler func(event *event.ItemCrafted)) {
	player.on(event.NameItemCrafted, func(e interface{}) {
		handler(e.(*event.ItemCrafted))
	})
}

// OnBlockPlaced subscribes to blocks placed by the player.
func (player *Player) OnBlockPlaced(handler func(event *event.BlockPlaced)) {
	player.on(event.NameBlockPlaced, func(e interface{}) {
		handler(e.(*event.BlockPlaced))
	})
}

// OnBlockBroken subscribes to blocks broken by the player.
func (player *Player) OnBlockBroken(handler func(event *event.BlockBroken)) {
	player.on(event.NameBlockBroken, func(e interface{}) {
		handler(e.(*event.BlockBroken))
	})
}

// OnPlayerMessage subscribes to player messages sent and received by the client. Note that an event
// is called both when the player chats and when the player receives its own chat, resulting in a duplicate
// event when the player chats.
func (player *Player) OnPlayerMessage(handler func(event *event.PlayerMessage)) {
	player.on(event.NamePlayerMessage, func(e interface{}) {
		messageEvent := e.(*event.PlayerMessage)
		if messageEvent.Message == player.lastReceivedMessage {
			// This is a hack for duplicated messages on the vanilla server.
			return
		}
		player.lastReceivedMessage = messageEvent.Message
		handler(messageEvent)
	})
}

// CloseConnection closes the socket connection with the player. The player will be closed shortly after this
// method is called.
func (player *Player) CloseConnection() {
	player.Exec("closewebsocket", nil)
}

// UnsubscribeFromAll unsubscribes from all events previously listened on. No more events will be received.
func (player *Player) UnsubscribeFromAll() {
	player.Lock()
	for eventName := range player.handlers {
		player.unsubscribeFrom(eventName)
	}
	player.Unlock()
}

// UnsubscribeFrom unsubscribes from events with the event name passed. The handler used to handle the event
// will no longer be called.
func (player *Player) UnsubscribeFrom(eventName event.Name) {
	player.Lock()
	player.unsubscribeFrom(eventName)
	player.Unlock()
}

// unsubscribeFrom unsubscribes from an event without locking.
func (player *Player) unsubscribeFrom(eventName event.Name) {
	_ = player.WriteJSON(protocol.NewEventRequest(eventName, protocol.Unsubscribe))
	delete(player.handlers, eventName)
}

// on subscribes to an arbitrary event. It is recommended to use the methods to listen specifically to events
// above.
func (player *Player) on(eventName event.Name, handler func(event interface{})) {
	player.Lock()
	player.handlers[eventName] = handler
	player.Unlock()
	_ = player.WriteJSON(protocol.NewEventRequest(eventName, protocol.Subscribe))
}

// WriteJSON adds a packet to the packet stack, after which will be written as JSON to the websocket
// connection.
func (player *Player) WriteJSON(v interface{}) error {
	player.packetStack <- v
	return nil
}

// sendPackets continuously sends packets to the websocket connection until the player is disconnected.
func (player *Player) sendPackets() {
	for {
		select {
		case packet := <-player.packetStack:
			_ = player.Conn.WriteJSON(packet)
		case <-player.close:
			return
		}
	}
}

// handleIncomingPacket handles an incoming packet, processing in particular the body of the packet.
func (player *Player) handleIncomingPacket(packet protocol.Packet) error {
	switch body := packet.Body.(type) {
	default:
		// Unknown or invalid packet. Don't try to process this.
		return fmt.Errorf("unknown packet %v", reflect.TypeOf(body).Name())
	case *protocol.ErrorResponse:
		return fmt.Errorf("a client side error occurred (code = %v): %v", body.StatusCode, body.StatusMessage)
	case *protocol.CommandResponse:
		player.Lock()
		callback, ok := player.commandCallbacks[packet.Header.RequestID]
		// Remove the command callback from the map.
		delete(player.commandCallbacks, packet.Header.RequestID)
		player.Unlock()
		if !ok {
			return fmt.Errorf("command response: got command response with unknown requestID %v", packet.Header.RequestID)
		}

		if callback.IsValid() {
			commandResponseValue := reflect.New(callback.Type().In(0)).Interface()
			if err := json.Unmarshal([]byte(*body), commandResponseValue); err != nil {
				return fmt.Errorf("command response: malformed response JSON %v: %v", string(*body), err)
			}
			callback.Call([]reflect.Value{reflect.ValueOf(commandResponseValue).Elem()})
		}
	case *protocol.EventResponse:
		properties := event.Properties{}
		if err := json.Unmarshal(body.Properties, &properties); err != nil {
			return fmt.Errorf("event response: malformed properties JSON: %v", err)
		}
		// Update the player's properties to the latest.
		player.Properties = properties

		eventData, ok := event.Events[body.EventName]
		if !ok {
			return fmt.Errorf("event response: unknown event with name %v", body.EventName)
		}
		_ = json.Unmarshal(body.Properties, &eventData)

		if measurable, ok := eventData.(event.Measurable); ok {
			// Parse measurements if the event requires them.
			measurable.ConsumeMeasurements(body.Measurements)
		}

		// Find the handler by the event name.
		player.Lock()
		handler, ok := player.handlers[body.EventName]
		player.Unlock()
		if !ok {
			return fmt.Errorf("event response: unhandled event response for event %v", body.EventName)
		}
		// Finally call the handler with the event data processed.
		handler(eventData)
	}
	return nil
}
