package controller

import (
	"encoding/json"
	"github.com/g3force/ssl-game-controller/pkg/timer"
	"io"
	"log"
	"os"
	"time"
)

const logDir = "logs"
const lastStateFileName = logDir + "/lastState.json"
const configFileName = "config/ssl-game-controller.yaml"

var RefBox = NewRefBox()

// GameController controls a game
type GameController struct {
	State            *State
	timer            timer.Timer
	MatchTimeStart   time.Time
	StateHistory     []State
	Config           Config
	stateHistoryFile *os.File
	lastStateFile    *os.File
	StageTimes       map[Stage]time.Duration
	Publisher        Publisher
	ApiServer        ApiServer
}

// NewRefBox creates a new RefBox
func NewRefBox() (refBox *GameController) {

	refBox = new(GameController)
	refBox.Config = loadConfig()
	refBox.ApiServer = ApiServer{}
	refBox.ApiServer.Consumer = refBox
	refBox.timer = timer.NewTimer()
	refBox.MatchTimeStart = time.Unix(0, 0)
	refBox.State = NewState(refBox.Config)
	refBox.Publisher = loadPublisher(refBox.Config)

	return
}

// Run the RefBox by loading configs, states, timer, etc.
func (r *GameController) Run() (err error) {

	os.MkdirAll(logDir, os.ModePerm)
	r.openStateFiles()
	r.readLastState()
	r.loadStages()
	r.timer.Start()

	go func() {
		if r.stateHistoryFile != nil {
			defer r.stateHistoryFile.Close()
		}
		if r.lastStateFile != nil {
			defer r.lastStateFile.Close()
		}
		for {
			r.timer.WaitTillNextFullSecond()
			r.Tick()
			r.Publish(nil)
		}
	}()
	return nil
}

func loadPublisher(config Config) Publisher {
	publisher, err := NewPublisher(config.Publish.Address)
	if err != nil {
		log.Printf("Could not start publisher on %v. %v", config.Publish.Address, err)
	}
	return publisher
}

func loadConfig() Config {
	config, err := LoadConfig(configFileName)
	if err != nil {
		log.Printf("Warning: Could not load config: %v", err)
	}
	return config
}

func (r *GameController) openStateFiles() {
	stateHistoryLogFileName := logDir + "/state-history_" + time.Now().Format("2006-01-02_15-04-05") + ".log"
	f, err := os.OpenFile(stateHistoryLogFileName, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		log.Fatal("Can not open state history log file", err)
	}
	r.stateHistoryFile = f
	f, err = os.OpenFile(lastStateFileName, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		log.Fatal("Can not open last state file", err)
	}
	r.lastStateFile = f
}

func (r *GameController) readLastState() {
	bufSize := 10000
	b := make([]byte, bufSize)
	n, err := r.lastStateFile.Read(b)
	if err != nil && err != io.EOF {
		log.Fatal("Could not read from last state file ", err)
	}
	if n == bufSize {
		log.Fatal("Buffer size too small")
	}
	if n > 0 {
		err = json.Unmarshal(b[:n], RefBox.State)
		if err != nil {
			log.Fatalf("Could not read last state: %v %v", string(b), err)
		}
	}
}

// Tick updates the times of the state and removes cards, if necessary
func (r *GameController) Tick() {
	delta := r.timer.Delta()
	updateTimes(r, delta)

	if r.MatchTimeStart.After(time.Unix(0, 0)) {
		r.State.MatchDuration = time.Now().Sub(r.MatchTimeStart)
	}
}

func (r *GameController) OnNewEvent(event Event) {

	err := processEvent(&event)
	if err != nil {
		log.Println("Could not process event:", event, err)
		return
	}

	r.Publish(event.Command)
}

// Publish publishes the state to the UI and the teams
func (r *GameController) Publish(command *EventCommand) {
	if command != nil {
		RefBox.SaveState()
	}
	r.ApiServer.PublishState(*RefBox.State)
	RefBox.Publisher.Publish(RefBox.State, command)
}

// SaveState writes the latest state out and logs the state history
func (r *GameController) SaveState() {
	r.SaveLatestState()
	r.SaveStateHistory()
}

// SaveLatestState writes the current state into a file
func (r *GameController) SaveLatestState() {
	jsonState, err := json.MarshalIndent(r.State, "", "  ")
	if err != nil {
		log.Print("Can not marshal state ", err)
		return
	}

	err = r.lastStateFile.Truncate(0)
	if err != nil {
		log.Fatal("Can not truncate last state file ", err)
	}
	_, err = r.lastStateFile.WriteAt(jsonState, 0)
	if err != nil {
		log.Print("Could not write last state ", err)
	}
	r.lastStateFile.Sync()
}

// SaveStateHistory writes the current state to the history file
func (r *GameController) SaveStateHistory() {

	r.StateHistory = append(r.StateHistory, *r.State)

	jsonState, err := json.Marshal(r.State)
	if err != nil {
		log.Print("Can not marshal state ", err)
		return
	}

	r.stateHistoryFile.Write(jsonState)
	r.stateHistoryFile.WriteString("\n")
	r.stateHistoryFile.Sync()
}

// UndoLastAction restores the last state from internal history
func (r *GameController) UndoLastAction() {
	lastIndex := len(r.StateHistory) - 2
	if lastIndex >= 0 {
		*r.State = r.StateHistory[lastIndex]
		r.StateHistory = r.StateHistory[0:lastIndex]
	}
}

func (r *GameController) loadStages() {
	r.StageTimes = map[Stage]time.Duration{}
	for _, stage := range Stages {
		r.StageTimes[stage] = 0
	}
	r.StageTimes[StageFirstHalf] = r.Config.Normal.HalfDuration
	r.StageTimes[StageHalfTime] = r.Config.Normal.HalfTimeDuration
	r.StageTimes[StageSecondHalf] = r.Config.Normal.HalfDuration
	r.StageTimes[StageOvertimeBreak] = r.Config.Normal.BreakAfter
	r.StageTimes[StageOvertimeFirstHalf] = r.Config.Overtime.HalfDuration
	r.StageTimes[StageOvertimeHalfTime] = r.Config.Overtime.HalfTimeDuration
	r.StageTimes[StageOvertimeSecondHalf] = r.Config.Overtime.HalfDuration
	r.StageTimes[StageShootoutBreak] = r.Config.Overtime.BreakAfter
}

func updateTimes(r *GameController, delta time.Duration) {
	if r.State.GameState == GameStateRunning {
		r.State.StageTimeElapsed += delta
		r.State.StageTimeLeft -= delta

		for _, teamState := range r.State.TeamState {
			reduceYellowCardTimes(teamState, delta)
			removeElapsedYellowCards(teamState)
		}
	}

	if r.State.GameState == GameStateTimeout && r.State.GameStateFor != nil {
		r.State.TeamState[*r.State.GameStateFor].TimeoutTimeLeft -= delta
	}
}

func reduceYellowCardTimes(teamState *TeamInfo, delta time.Duration) {
	for i := range teamState.YellowCardTimes {
		teamState.YellowCardTimes[i] -= delta
	}
}

func removeElapsedYellowCards(teamState *TeamInfo) {
	b := teamState.YellowCardTimes[:0]
	for _, x := range teamState.YellowCardTimes {
		if x > 0 {
			b = append(b, x)
		}
	}
	teamState.YellowCardTimes = b
}
