#![deny(warnings)]

use {
    clap::Parser,
    futures::{FutureExt, TryStreamExt, stream::FuturesUnordered},
    std::time::Duration,
    tokio::fs,
    wasmtime::{
        AsContextMut, Config, Engine, Store,
        component::{Accessor, Component, HasData, Linker, ResourceTable},
    },
    wasmtime_wasi::{WasiCtx, WasiCtxBuilder, WasiCtxView, WasiView},
};

pub mod bindings {
    wasmtime::component::bindgen!({
        path: "../wit",
        world: "round-trip",
    });
}

#[derive(Parser)]
#[command(version, about)]
struct Cli {
    /// Component to test
    component: String,
}

pub struct Ctx {
    pub wasi: WasiCtx,
    pub table: ResourceTable,
}

impl WasiView for Ctx {
    fn ctx(&mut self) -> WasiCtxView<'_> {
        WasiCtxView {
            ctx: &mut self.wasi,
            table: &mut self.table,
        }
    }
}

impl HasData for Ctx {
    type Data<'a> = &'a mut Self;
}

impl bindings::local::local::baz::HostWithStore for Ctx {
    async fn foo<T>(_: &Accessor<T, Self>, s: String) -> String {
        tokio::time::sleep(Duration::from_millis(10)).await;
        format!("host[{s}]")
    }
}

impl bindings::local::local::baz::Host for Ctx {}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let cli = Cli::parse();

    let mut config = Config::new();
    config.wasm_component_model(true);
    config.wasm_component_model_async(true);
    config.async_support(true);

    let engine = Engine::new(&config)?;
    let mut linker = Linker::new(&engine);

    wasmtime_wasi::p2::add_to_linker_async(&mut linker)?;
    bindings::local::local::baz::add_to_linker::<_, Ctx>(&mut linker, |ctx| ctx)?;

    let mut store = Store::new(
        &engine,
        Ctx {
            wasi: WasiCtxBuilder::new().inherit_stdio().build(),
            table: ResourceTable::default(),
        },
    );

    let component = Component::new(&engine, &fs::read(&cli.component).await?)?;

    let guest = bindings::RoundTrip::instantiate_async(&mut store, &component, &linker).await?;

    let inputs_and_outputs = &[
        (
            "hello, world!",
            "guest[host[hello, world!], host[hello, world!], host[hello, world!]]",
        ),
        (
            "¡hola, mundo!",
            "guest[host[¡hola, mundo!], host[¡hola, mundo!], host[¡hola, mundo!]]",
        ),
        (
            "hi y'all!",
            "guest[host[hi y'all!], host[hi y'all!], host[hi y'all!]]",
        ),
    ];

    store
        .as_context_mut()
        .run_concurrent({
            let inputs_and_outputs = inputs_and_outputs
                .iter()
                .map(|(a, b)| (String::from(*a), String::from(*b)))
                .collect::<Vec<_>>();

            async move |accessor| {
                let mut futures = FuturesUnordered::new();
                for (input, output) in &inputs_and_outputs {
                    let output = output.clone();
                    futures.push(
                        guest
                            .local_local_baz()
                            .call_foo(accessor, input.clone())
                            .map(move |v| v.map(move |v| (v, output)))
                            .boxed(),
                    );
                }

                while let Some((actual, expected)) = futures.try_next().await? {
                    assert_eq!(expected, actual);
                }

                Ok::<_, wasmtime::Error>(())
            }
        })
        .await?
}
