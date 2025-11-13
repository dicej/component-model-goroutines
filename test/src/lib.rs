#![deny(warnings)]

#[cfg(test)]
mod test {
    use {
        futures::{FutureExt, TryStreamExt, stream::FuturesUnordered},
        std::{
            env, mem,
            ops::DerefMut,
            pin::Pin,
            sync::{Arc, Mutex},
            task::{self, Context, Poll},
            time::Duration,
        },
        tokio::fs,
        wasmtime::{
            AsContextMut, Config, Engine, Store, StoreContextMut,
            component::{
                Accessor, Component, Destination, HasData, Lift, Linker, ResourceTable, Source,
                StreamConsumer, StreamProducer, StreamReader, StreamResult, VecBuffer,
            },
        },
        wasmtime_wasi::{WasiCtx, WasiCtxBuilder, WasiCtxView, WasiView},
    };

    pub mod bindings {
        wasmtime::component::bindgen!({
            path: "../wit",
            world: "round-trip",
            exports: { default: async | task_exit },
        });
    }

    const DELAY: Duration = Duration::from_millis(100);

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
        async fn foo<T>(_: &Accessor<T, Self>, s: String, delay: bool) -> String {
            if delay {
                tokio::time::sleep(DELAY).await;
            }
            format!("host[{s}]")
        }
    }

    impl bindings::local::local::baz::Host for Ctx {}

    fn engine() -> anyhow::Result<Engine> {
        let mut config = Config::new();
        config.wasm_component_model(true);
        config.wasm_component_model_async(true);
        config.async_support(true);

        Engine::new(&config)
    }

    async fn run_test(
        fun: impl AsyncFnOnce(&Accessor<Ctx>, &bindings::RoundTrip) -> anyhow::Result<()>,
    ) -> anyhow::Result<()> {
        let engine = engine()?;
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

        let component = Component::new(&engine, &fs::read(&env::var("TEST_COMPONENT")?).await?)?;

        let guest = bindings::RoundTrip::instantiate_async(&mut store, &component, &linker).await?;

        store
            .as_context_mut()
            .run_concurrent(async move |accessor| fun(accessor, &guest).await)
            .await?
    }

    #[tokio::test]
    async fn round_trip() -> anyhow::Result<()> {
        test_round_trip(false).await
    }

    #[tokio::test]
    async fn round_trip_with_delay() -> anyhow::Result<()> {
        test_round_trip(true).await
    }

    async fn test_round_trip(delay: bool) -> anyhow::Result<()> {
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

        run_test({
            let inputs_and_outputs = inputs_and_outputs
                .iter()
                .map(|(a, b)| (String::from(*a), String::from(*b)))
                .collect::<Vec<_>>();

            async move |accessor, guest| {
                let mut futures = FuturesUnordered::new();
                for (input, output) in &inputs_and_outputs {
                    let output = output.clone();
                    futures.push(
                        guest
                            .local_local_baz()
                            .call_foo(accessor, input.clone(), delay)
                            .map(move |v| v.map(move |(v, _)| (v, output)))
                            .boxed(),
                    );
                }

                while let Some((actual, expected)) = futures.try_next().await? {
                    assert_eq!(expected, actual);
                }

                Ok(())
            }
        })
        .await
    }

    struct VecProducer<T> {
        source: Vec<T>,
        sleep: Pin<Box<dyn Future<Output = ()> + Send>>,
    }

    impl<T> VecProducer<T> {
        fn new(source: Vec<T>, delay: bool) -> Self {
            Self {
                source,
                sleep: if delay {
                    tokio::time::sleep(DELAY).boxed()
                } else {
                    async {}.boxed()
                },
            }
        }
    }

    impl<D, T: Lift + Unpin + 'static> StreamProducer<D> for VecProducer<T> {
        type Item = T;
        type Buffer = VecBuffer<T>;

        fn poll_produce(
            mut self: Pin<&mut Self>,
            cx: &mut Context<'_>,
            _: StoreContextMut<D>,
            mut destination: Destination<Self::Item, Self::Buffer>,
            _: bool,
        ) -> Poll<anyhow::Result<StreamResult>> {
            let sleep = &mut self.as_mut().get_mut().sleep;
            task::ready!(sleep.as_mut().poll(cx));
            *sleep = async {}.boxed();

            destination.set_buffer(mem::take(&mut self.get_mut().source).into());
            Poll::Ready(Ok(StreamResult::Dropped))
        }
    }

    struct VecConsumer<T> {
        destination: Arc<Mutex<Vec<T>>>,
        sleep: Pin<Box<dyn Future<Output = ()> + Send>>,
    }

    impl<T> VecConsumer<T> {
        fn new(destination: Arc<Mutex<Vec<T>>>, delay: bool) -> Self {
            Self {
                destination,
                sleep: if delay {
                    tokio::time::sleep(DELAY).boxed()
                } else {
                    async {}.boxed()
                },
            }
        }
    }

    impl<D, T: Lift + 'static> StreamConsumer<D> for VecConsumer<T> {
        type Item = T;

        fn poll_consume(
            mut self: Pin<&mut Self>,
            cx: &mut Context<'_>,
            store: StoreContextMut<D>,
            mut source: Source<Self::Item>,
            _: bool,
        ) -> Poll<anyhow::Result<StreamResult>> {
            let sleep = &mut self.as_mut().get_mut().sleep;
            task::ready!(sleep.as_mut().poll(cx));
            *sleep = async {}.boxed();

            source.read(store, self.destination.lock().unwrap().deref_mut())?;
            Poll::Ready(Ok(StreamResult::Completed))
        }
    }

    #[tokio::test]
    async fn read_stream_u8() -> anyhow::Result<()> {
        test_read_stream_u8(false).await
    }

    #[tokio::test]
    async fn read_stream_u8_with_delay() -> anyhow::Result<()> {
        test_read_stream_u8(true).await
    }

    async fn test_read_stream_u8(delay: bool) -> anyhow::Result<()> {
        run_test(async move |accessor, guest| {
            let expected = b"Beware the Jubjub bird, and shun\n\tThe frumious Bandersnatch!";
            let stream = accessor.with(|access| {
                StreamReader::new(access, VecProducer::new(expected.to_vec(), delay))
            });

            let (list, task) = guest
                .local_local_streams_and_futures()
                .call_read_stream_u8(accessor, stream)
                .await?;

            task.block(accessor).await;

            assert_eq!(expected, &list[..]);

            Ok(())
        })
        .await
    }

    #[tokio::test]
    async fn echo_stream_u8() -> anyhow::Result<()> {
        test_echo_stream_u8(false).await
    }

    #[tokio::test]
    async fn echo_stream_u8_with_delay() -> anyhow::Result<()> {
        test_echo_stream_u8(true).await
    }

    async fn test_echo_stream_u8(delay: bool) -> anyhow::Result<()> {
        run_test(async move |accessor, guest| {
            let expected = b"Beware the Jubjub bird, and shun\n\tThe frumious Bandersnatch!";
            let stream = accessor.with(|access| {
                StreamReader::new(access, VecProducer::new(expected.to_vec(), delay))
            });

            let (stream, task) = guest
                .local_local_streams_and_futures()
                .call_echo_stream_u8(accessor, stream)
                .await?;

            let received = Arc::new(Mutex::new(Vec::with_capacity(expected.len())));
            accessor.with(|access| stream.pipe(access, VecConsumer::new(received.clone(), delay)));

            task.block(accessor).await;

            assert_eq!(expected, &received.lock().unwrap()[..]);

            Ok(())
        })
        .await
    }
}
