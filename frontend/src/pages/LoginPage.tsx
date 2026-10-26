import { Alert, Button, Center, Divider, Paper, PasswordInput, Stack, TextInput, Title } from "@mantine/core";
import { IconKey } from "@tabler/icons-react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { ApiError, errorText } from "../api";
import { fetchSignInOptions, login, meQueryKey, providerStartUrl, safeNext } from "../auth";
import { ErrorState, PageLoader } from "../components/States";
import { t } from "../i18n";

// Local sign-in form and one button per OIDC provider (E9-1), whichever
// the instance has. A failed provider flow lands back here as ?error=code.
export default function LoginPage() {
  const [params] = useSearchParams();
  const next = safeNext(params.get("next"));
  const flowError = params.get("error");
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

  const options = useQuery({ queryKey: ["signInOptions"], queryFn: ({ signal }) => fetchSignInOptions(signal) });
  const signIn = useMutation({
    mutationFn: () => login(username.trim(), password),
    onSuccess: (user) => {
      queryClient.setQueryData(meQueryKey, user);
      navigate(next, { replace: true });
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    signIn.mutate();
  };

  return (
    <Center py="xl">
      <Paper withBorder shadow="sm" p="xl" w="100%" maw={400}>
        <Stack>
          <Title order={2} ta="center">
            {t("login.title")}
          </Title>
          {flowError && !signIn.isError && (
            <Alert color="red">{errorText(new ApiError(0, flowError, "sign-in flow failed"))}</Alert>
          )}
          {options.isPending && <PageLoader compact />}
          {options.isError && <ErrorState compact error={options.error} onRetry={() => void options.refetch()} />}
          {options.data?.password && (
            <form onSubmit={submit}>
              <Stack>
                <TextInput
                  label={t("login.username")}
                  autoComplete="username"
                  value={username}
                  onChange={(e) => setUsername(e.currentTarget.value)}
                  required
                  autoFocus
                />
                <PasswordInput
                  label={t("login.password")}
                  autoComplete="current-password"
                  value={password}
                  onChange={(e) => setPassword(e.currentTarget.value)}
                  required
                />
                {signIn.isError && <Alert color="red">{errorText(signIn.error)}</Alert>}
                <Button type="submit" loading={signIn.isPending}>
                  {t("login.submit")}
                </Button>
              </Stack>
            </form>
          )}
          {options.data && options.data.password && options.data.providers.length > 0 && (
            <Divider label={t("login.or")} labelPosition="center" />
          )}
          {options.data?.providers.map((p) => (
            <Button key={p.slug} component="a" href={providerStartUrl(p.slug, next)} variant="default" leftSection={<IconKey size={16} />}>
              {t("login.withProvider", { name: p.name })}
            </Button>
          ))}
          {options.data && !options.data.password && options.data.providers.length === 0 && (
            <Alert color="yellow">{t("login.noMethods")}</Alert>
          )}
        </Stack>
      </Paper>
    </Center>
  );
}
