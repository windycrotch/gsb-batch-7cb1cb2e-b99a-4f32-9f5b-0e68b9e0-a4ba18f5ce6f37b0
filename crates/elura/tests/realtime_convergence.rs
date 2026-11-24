#![cfg(all(feature = "core", feature = "netcode"))]

//! Convergence tests between the authoritative realtime room and the client
//! prediction buffer.
//!
//! Both sides run the same deterministic rules: an input adds its value, then
//! every tick advances the position by one. The room publishes whole-state
//! keyframes through an in-process bridge; the client treats those as the
//! authoritative states used for reconciliation.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use async_trait::async_trait;

use elura::core::realtime::{
    AuthoritativeSimulation, Command, Room, RoomConfig, Snapshot, SnapshotPublisher, Tick,
};
use elura::core::session::PlayerKey;
use elura::core::state_hash::StateHash;
use elura::gameplay::netcode::{
    NetcodeError, PredictionBuffer, PredictionConfig, ReconciliationReport,
};
use elura::{Error, Result};

#[derive(Clone, Debug, PartialEq, Eq)]
struct Move(i64);

#[derive(Clone, Debug, PartialEq, Eq)]
struct WorldState {
    position: i64,
}

fn encode(state: &WorldState) -> Vec<u8> {
    state.position.to_le_bytes().to_vec()
}

/// Room-side deterministic simulation.
struct RoomSim {
    position: i64,
    fail_restore: Option<Arc<AtomicBool>>,
}

#[async_trait]
impl AuthoritativeSimulation for RoomSim {
    type Command = Move;
    type Snapshot = WorldState;

    async fn apply(&mut self, command: Command<Move>) -> Result<()> {
        self.position += command.value.0;
        Ok(())
    }

    async fn step(&mut self, _tick: Tick) -> Result<()> {
        self.position += 1;
        Ok(())
    }

    async fn snapshot(&self, _tick: Tick) -> Result<WorldState> {
        Ok(WorldState {
            position: self.position,
        })
    }

    fn encode_snapshot(&self, snapshot: &WorldState) -> Result<Vec<u8>> {
        Ok(encode(snapshot))
    }

    async fn restore(&mut self, _tick: Tick, snapshot: WorldState) -> Result<()> {
        if self
            .fail_restore
            .as_ref()
            .is_some_and(|flag| flag.load(Ordering::Acquire))
        {
            return Err(Error::Unavailable);
        }
        self.position = snapshot.position;
        Ok(())
    }
}

/// One authoritative snapshot as observed by a client.
#[derive(Clone, Debug, PartialEq, Eq)]
struct Authoritative {
    tick: u64,
    state: WorldState,
    hash: StateHash,
}

/// In-process stand-in for the snapshot replication channel.
#[derive(Default)]
struct SnapshotBridge {
    latest: Mutex<Option<Authoritative>>,
    history: Mutex<Vec<Authoritative>>,
    forced: AtomicBool,
}

impl SnapshotBridge {
    fn latest(&self) -> Authoritative {
        self.latest
            .lock()
            .unwrap()
            .clone()
            .expect("published snapshot")
    }

    fn history(&self) -> Vec<Authoritative> {
        self.history.lock().unwrap().clone()
    }
}

#[async_trait]
impl SnapshotPublisher<WorldState> for SnapshotBridge {
    async fn publish(&self, snapshot: Snapshot<WorldState>) -> Result<()> {
        let published = Authoritative {
            tick: snapshot.tick,
            state: snapshot.state,
            hash: snapshot.state_hash,
        };
        self.latest.lock().unwrap().replace(published.clone());
        self.history.lock().unwrap().push(published);
        Ok(())
    }

    fn force_keyframe(&self) {
        self.forced.store(true, Ordering::Release);
    }
}

fn player(user_id: i64) -> PlayerKey {
    PlayerKey::new(1, 1, user_id).unwrap()
}

fn room_config() -> RoomConfig {
    let mut config = RoomConfig::default();
    config.input_delay_ticks = 0;
    config.rollback_window_ticks = 8;
    config.snapshot_interval = 1;
    config
}

/// Client rule: an input and a tick both move the position, matching the room.
fn replay_input(state: &mut WorldState, input: &Move) {
    state.position += input.0 + 1;
}

/// Records one locally predicted frame and sends the same input to the room.
async fn predict_and_send(
    room: &Room<RoomSim>,
    prediction: &mut PredictionBuffer<Move, WorldState>,
    current: &mut WorldState,
    tick: u64,
    sequence: u64,
    input: Move,
) {
    replay_input(current, &input);
    prediction
        .record(tick, input.clone(), current.clone())
        .unwrap();
    room.submit_at(
        tick,
        Command {
            player: player(1),
            sequence,
            value: input,
        },
    )
    .await
    .unwrap();
}

fn deliver(
    prediction: &mut PredictionBuffer<Move, WorldState>,
    bridge: &SnapshotBridge,
    expected_tick: u64,
) -> ReconciliationReport<WorldState> {
    let authoritative = bridge.latest();
    assert_eq!(authoritative.tick, expected_tick);
    prediction
        .reconcile(
            authoritative.tick,
            authoritative.state,
            |state, _, input| replay_input(state, input),
        )
        .expect("authoritative correction accepted")
}

#[tokio::test]
async fn late_input_rollback_converges_room_and_client_on_one_tick() {
    let bridge = Arc::new(SnapshotBridge::default());
    let room = Room::new(
        "room",
        room_config(),
        RoomSim {
            position: 0,
            fail_restore: None,
        },
    )
    .unwrap()
    .with_publisher(Arc::clone(&bridge) as Arc<dyn SnapshotPublisher<WorldState>>);
    let mut prediction = PredictionBuffer::new(PredictionConfig::default()).unwrap();
    let mut client_state = WorldState { position: 0 };

    // Ticks 1..=3: the client predicts ahead and confirms each room snapshot.
    for tick in 1..=3 {
        predict_and_send(
            &room,
            &mut prediction,
            &mut client_state,
            tick,
            tick,
            Move(0),
        )
        .await;
        room.advance().await.unwrap();
        let report = deliver(&mut prediction, &bridge, tick);
        assert_eq!(report.authoritative_tick, tick);
        assert_eq!(prediction.confirmed_tick(), tick);
    }
    assert_eq!(room.current_tick(), 3);

    // The client predicts ticks 4..=6 before the room reaches them.
    for tick in 4..=6 {
        predict_and_send(
            &room,
            &mut prediction,
            &mut client_state,
            tick,
            tick,
            Move(0),
        )
        .await;
    }

    // A late input for tick 1 forces a room rollback across ticks 1..=3.
    room.submit_at(
        1,
        Command {
            player: player(2),
            sequence: 1,
            value: Move(5),
        },
    )
    .await
    .unwrap();
    room.advance().await.unwrap();

    let stats = room.stats();
    assert_eq!(stats.rollbacks_completed, 1);
    assert_eq!(stats.rollback_failures, 0);
    assert!(bridge.forced.load(Ordering::Acquire));

    // The rollback advance publishes exactly once, at the final tick, and the
    // published hash matches the canonical encoding of its state.
    let published = bridge.history();
    let keyframe = published
        .iter()
        .find(|snapshot| snapshot.tick == 4)
        .expect("rollback keyframe at tick 4");
    assert_eq!(
        published
            .iter()
            .filter(|snapshot| snapshot.tick == 4)
            .count(),
        1
    );
    assert!(keyframe.hash.matches(&encode(&keyframe.state)));
    // Replayed timeline: tick1 applies the late +5 then steps to 6, tick4 is 9.
    assert_eq!(keyframe.state.position, 9);

    // Client correction at authoritative tick 4 replays only its later,
    // still-unconfirmed inputs for ticks 5 and 6.
    let report = deliver(&mut prediction, &bridge, 4);
    assert_eq!(report.authoritative_tick, 4);
    assert_eq!(report.replayed_inputs, 2);
    assert_eq!(report.current_tick, 6);
    assert_eq!(report.corrected_state.position, 11);
    assert_eq!(prediction.confirmed_tick(), 4);

    // The room then advances through the client's predicted ticks: the current
    // tick and position must converge exactly.
    room.advance().await.unwrap();
    room.advance().await.unwrap();
    assert_eq!(room.current_tick(), report.current_tick);
    let latest = bridge.latest();
    assert_eq!(latest.tick, 6);
    assert_eq!(latest.state, report.corrected_state);
    assert!(latest.hash.matches(&encode(&latest.state)));

    let final_report = deliver(&mut prediction, &bridge, 6);
    assert_eq!(final_report.current_tick, 6);
    assert_eq!(final_report.corrected_state.position, 11);
    assert!(prediction.is_empty());
}

#[tokio::test]
async fn failed_room_rollback_never_becomes_a_confirmed_client_tick() {
    let fail_restore = Arc::new(AtomicBool::new(false));
    let bridge = Arc::new(SnapshotBridge::default());
    let room = Room::new(
        "room",
        room_config(),
        RoomSim {
            position: 0,
            fail_restore: Some(Arc::clone(&fail_restore)),
        },
    )
    .unwrap()
    .with_publisher(Arc::clone(&bridge) as Arc<dyn SnapshotPublisher<WorldState>>);
    let mut prediction = PredictionBuffer::new(PredictionConfig::default()).unwrap();
    let mut client_state = WorldState { position: 0 };

    for tick in 1..=3 {
        predict_and_send(
            &room,
            &mut prediction,
            &mut client_state,
            tick,
            tick,
            Move(0),
        )
        .await;
        room.advance().await.unwrap();
        deliver(&mut prediction, &bridge, tick);
    }
    let last_good = bridge.latest();
    assert_eq!(last_good.tick, 3);

    // The late input cannot be resimulated because restore fails.
    fail_restore.store(true, Ordering::Release);
    room.submit_at(
        1,
        Command {
            player: player(2),
            sequence: 1,
            value: Move(9),
        },
    )
    .await
    .unwrap();
    let error = room.advance().await;
    assert!(matches!(error, Err(Error::Unavailable)));

    // The failed tick does not advance the room and produces no publication.
    assert_eq!(room.current_tick(), 3);
    assert_eq!(room.stats().rollbacks_completed, 0);
    assert_eq!(room.stats().rollback_failures, 1);
    assert_eq!(bridge.latest(), last_good);

    // With no new authoritative state, the client cannot confirm anything new.
    assert_eq!(prediction.confirmed_tick(), 3);
    let frames_before: Vec<_> = prediction
        .frames()
        .map(|frame| (frame.tick, frame.predicted_state.clone()))
        .collect();
    assert!(frames_before.is_empty());

    // Recovery: clear the fault, advance normally and reconcile at the new tick.
    fail_restore.store(false, Ordering::Release);
    room.advance().await.unwrap();
    assert_eq!(room.current_tick(), 4);
    let report = deliver(&mut prediction, &bridge, 4);
    assert_eq!(report.authoritative_tick, 4);
    assert_eq!(prediction.confirmed_tick(), 4);
    assert_eq!(bridge.latest().state.position, 4);
}

#[test]
fn duplicate_or_older_authoritative_states_are_rejected_without_buffer_changes() {
    let mut prediction = PredictionBuffer::new(PredictionConfig::default()).unwrap();
    for tick in 1..=5 {
        prediction
            .record(
                tick,
                Move(0),
                WorldState {
                    position: tick as i64,
                },
            )
            .unwrap();
    }
    let authoritative = |tick: u64| WorldState {
        position: tick as i64,
    };
    prediction
        .reconcile(2, authoritative(2), |state, _, input| {
            replay_input(state, input)
        })
        .unwrap();
    assert_eq!(prediction.confirmed_tick(), 2);
    let frames_before: Vec<_> = prediction
        .frames()
        .map(|frame| (frame.tick, frame.predicted_state.clone()))
        .collect();

    // Repeated authoritative tick.
    let repeated = prediction.reconcile(2, authoritative(2), |state, _, input| {
        replay_input(state, input)
    });
    assert!(matches!(repeated, Err(NetcodeError::InvalidInput(_))));
    // Older authoritative tick.
    let older = prediction.reconcile(1, authoritative(1), |state, _, input| {
        replay_input(state, input)
    });
    assert!(matches!(older, Err(NetcodeError::InvalidInput(_))));

    assert_eq!(prediction.confirmed_tick(), 2);
    let frames_after: Vec<_> = prediction
        .frames()
        .map(|frame| (frame.tick, frame.predicted_state.clone()))
        .collect();
    assert_eq!(frames_after, frames_before);
}

#[test]
fn replay_overflow_waits_for_a_later_authoritative_state() {
    let mut config = PredictionConfig::default();
    config.history_capacity = 16;
    config.max_replay_steps = 2;
    let mut prediction = PredictionBuffer::new(config).unwrap();
    for tick in 1..=5 {
        prediction
            .record(
                tick,
                Move(0),
                WorldState {
                    position: tick as i64,
                },
            )
            .unwrap();
    }

    // Four unconfirmed inputs exceed the two-step replay ceiling: the
    // correction is refused and the buffer stays intact.
    let overflow = prediction.reconcile(1, WorldState { position: 1 }, |state, _, input| {
        replay_input(state, input)
    });
    assert_eq!(overflow, Err(NetcodeError::ReplayLimitExceeded));
    assert_eq!(prediction.confirmed_tick(), 0);
    assert_eq!(prediction.pending_len(), 5);

    // A later authoritative state fits the ceiling and is accepted.
    let report = prediction
        .reconcile(4, WorldState { position: 4 }, |state, _, input| {
            replay_input(state, input)
        })
        .unwrap();
    assert_eq!(report.replayed_inputs, 1);
    assert_eq!(report.current_tick, 5);
    assert_eq!(report.corrected_state.position, 5);
    assert_eq!(prediction.confirmed_tick(), 4);
    assert_eq!(prediction.pending_len(), 1);
}

#[tokio::test]
async fn concurrent_advances_publish_strictly_monotonic_keyframes() {
    let bridge = Arc::new(SnapshotBridge::default());
    let mut concurrent_config = room_config();
    concurrent_config.rollback_window_ticks = 32;
    let room = Arc::new(
        Room::new(
            "room",
            concurrent_config,
            RoomSim {
                position: 0,
                fail_restore: None,
            },
        )
        .unwrap()
        .with_publisher(Arc::clone(&bridge) as Arc<dyn SnapshotPublisher<WorldState>>),
    );

    let mut handles = Vec::new();
    for _ in 0..16 {
        let room = Arc::clone(&room);
        handles.push(tokio::spawn(async move { room.advance().await }));
    }
    for handle in handles {
        handle.await.unwrap().unwrap();
    }

    assert_eq!(room.current_tick(), 16);
    assert_eq!(room.stats().tick_failures, 0);

    // Every advance published once, ticks strictly increasing, each hash valid.
    let published = bridge.history();
    assert_eq!(published.len(), 16);
    for (index, snapshot) in published.iter().enumerate() {
        assert_eq!(snapshot.tick, index as u64 + 1);
        assert!(snapshot.hash.matches(&encode(&snapshot.state)));
        assert_eq!(snapshot.state.position, snapshot.tick as i64);
    }

    // The client reconciles on the final keyframe without any replay work.
    let mut prediction = PredictionBuffer::new(PredictionConfig::default()).unwrap();
    let report = deliver(&mut prediction, &bridge, 16);
    assert_eq!(report.authoritative_tick, 16);
    assert_eq!(report.current_tick, 16);
    assert_eq!(report.corrected_state.position, 16);
    assert_eq!(prediction.confirmed_tick(), room.current_tick());
}

#[tokio::test]
async fn reconnect_rebases_a_divergent_client_on_the_latest_keyframe() {
    let bridge = Arc::new(SnapshotBridge::default());
    let room = Room::new(
        "room",
        room_config(),
        RoomSim {
            position: 0,
            fail_restore: None,
        },
    )
    .unwrap()
    .with_publisher(Arc::clone(&bridge) as Arc<dyn SnapshotPublisher<WorldState>>);

    for _ in 0..=5 {
        room.advance().await.unwrap();
    }
    assert_eq!(room.current_tick(), 6);

    // The reconnecting client carries divergent local predictions.
    let mut prediction = PredictionBuffer::new(PredictionConfig::default()).unwrap();
    prediction
        .record(5, Move(40), WorldState { position: 999 })
        .unwrap();
    prediction
        .record(6, Move(40), WorldState { position: 1040 })
        .unwrap();

    // Reconnection discards local history and installs the room keyframe.
    prediction.reset(0);
    let report = deliver(&mut prediction, &bridge, 6);
    assert_eq!(report.corrected_state, WorldState { position: 6 });
    assert_eq!(prediction.confirmed_tick(), 6);
    assert!(prediction.is_empty());

    // Convergence holds on the next tick with a shared input.
    let mut client_state = report.corrected_state;
    predict_and_send(&room, &mut prediction, &mut client_state, 7, 7, Move(2)).await;
    room.advance().await.unwrap();
    let report = deliver(&mut prediction, &bridge, 7);
    assert_eq!(report.corrected_state.position, 9);
    assert_eq!(bridge.latest().state.position, 9);
    assert_eq!(prediction.confirmed_tick(), room.current_tick());
}
