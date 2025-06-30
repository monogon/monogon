/*
 * Copyright The Monogon Project Authors.
 * SPDX-License-Identifier: Apache-2.0
 */

#![no_main]
#![no_std]

extern crate alloc;

use alloc::vec::Vec;
use core::fmt;
use core::result::Result;
use prost::Message;
use uefi::boot::{self, ScopedProtocol};
use uefi::fs::FileSystem;
use uefi::proto::device_path::build::media::FilePath;
use uefi::proto::device_path::build::DevicePathBuilder;
use uefi::proto::device_path::{DeviceSubType, DeviceType, LoadedImageDevicePath};
use uefi::proto::media::fs::SimpleFileSystem;
use uefi::{prelude::*, CStr16};
use uefi::println;

use abloader_proto::metropolis::node::abloader::spec::*;

const A_LOADER_PATH: &CStr16 = cstr16!("\\EFI\\metropolis\\boot-a.efi");
const B_LOADER_PATH: &CStr16 = cstr16!("\\EFI\\metropolis\\boot-b.efi");

const LOADER_STATE_PATH: &CStr16 = cstr16!("\\EFI\\metropolis\\loader_state.pb");

enum ValidSlot {
    A,
    B,
}

impl ValidSlot {
    // other returns B if the value is A and A if the value is B.
    fn other(&self) -> Self {
        match self {
            ValidSlot::A => ValidSlot::B,
            ValidSlot::B => ValidSlot::A,
        }
    }
    // path returns the path to the slot's EFI payload.
    fn path(&self) -> &'static CStr16 {
        match self {
            ValidSlot::A => A_LOADER_PATH,
            ValidSlot::B => B_LOADER_PATH,
        }
    }
}

enum ReadLoaderStateError {
    FSReadError(uefi::fs::Error),
    DecodeError(prost::DecodeError),
}

impl fmt::Display for ReadLoaderStateError {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        match self {
            ReadLoaderStateError::FSReadError(e) => write!(f, "while reading state file: {}", e),
            ReadLoaderStateError::DecodeError(e) => write!(f, "while decoding state file contents: {}", e),
        }
    }
}

fn read_loader_state(fs: &mut FileSystem) -> Result<AbLoaderData, ReadLoaderStateError> {
    let state_raw = fs.read(&LOADER_STATE_PATH).map_err(|e| ReadLoaderStateError::FSReadError(e))?;
    AbLoaderData::decode(state_raw.as_slice()).map_err(|e| ReadLoaderStateError::DecodeError(e))
}

fn load_slot_image(slot: &ValidSlot) -> uefi::Result<Handle> {
    let mut storage = Vec::new();

    // Build the path to the slot payload. This takes the path to the loader
    // itself, strips off the file path and following element(s) and appends
    // the path to the correct slot payload.
    let new_image_path = {
        let loaded_image_device_path =
            boot::open_protocol_exclusive::<LoadedImageDevicePath>(boot::image_handle())?;

        let mut builder = DevicePathBuilder::with_vec(&mut storage);

        for node in loaded_image_device_path.node_iter() {
            if node.full_type() == (DeviceType::MEDIA, DeviceSubType::MEDIA_FILE_PATH) {
                break;
            }

            builder = builder.push(&node).unwrap();
        }

        builder = builder
            .push(&FilePath {
                path_name: slot.path(),
            })
            .unwrap();

        builder.finalize().unwrap()
    };

    boot::load_image(
        boot::image_handle(),
        boot::LoadImageSource::FromDevicePath {
            device_path: new_image_path,
            boot_policy: uefi::proto::BootPolicy::ExactMatch,
        },
    )
}

#[entry]
fn main() -> Status {
    let boot_slot_raw = {
        let esp_fs: ScopedProtocol<SimpleFileSystem> =
            boot::get_image_file_system(boot::image_handle())
                .expect("image filesystem not available");
        let mut esp_fs = FileSystem::new(esp_fs);

        let mut loader_data = match read_loader_state(&mut esp_fs) {
            Ok(d) => d,
            Err(e) => {
                println!("Unable to load A/B loader state, using default slot A: {}", e);
                AbLoaderData {
                    active_slot: Slot::A.into(),
                    next_slot: Slot::None.into(),
                }
            }
        };

        // If next_slot is set, use it as slot to boot but clear it in the
        // state file as the next boot should not use it again. If it should
        // be permanently activated, it is the OS's job to put it into
        if loader_data.next_slot != Slot::None.into() {
            let next_slot = loader_data.next_slot;
            loader_data.next_slot = Slot::None.into();
            let new_loader_data = loader_data.encode_to_vec();
            esp_fs
                .write(&LOADER_STATE_PATH, new_loader_data)
                .expect("failed to write back abdata");
            next_slot
        } else {
            loader_data.active_slot
        }
    };

    let boot_slot = match Slot::try_from(boot_slot_raw) {
        Ok(Slot::A) => ValidSlot::A,
        Ok(Slot::B) => ValidSlot::B,
        _ => {
            println!("Invalid slot ({}) active, falling back to A", boot_slot_raw);
            ValidSlot::A
        }
    };

    let payload_image = match load_slot_image(&boot_slot) {
        Ok(img) => img,
        Err(e) => {
            println!("Error loading intended slot, falling back to other slot: {}", e);
            match load_slot_image(&boot_slot.other()) {
                Ok(img) => img,
                Err(e) => {
                    panic!("Loading from both slots failed, second slot error: {}", e);
                },
            }
        }
    };

    // Boot the payload.
    boot::start_image(payload_image).expect("failed to start payload");
    Status::SUCCESS
}
