package es.tracklin.Client;

import es.tracklin.DataConnections.AccountService;
import org.junit.Rule;
import org.junit.Test;
import org.mockito.Mock;
import org.mockito.junit.MockitoJUnit;
import org.mockito.junit.MockitoRule;

import static org.junit.Assert.assertNotEquals;

public class AccountCreationTests {
    @Mock
    private AccountService accountService;

    @Mock
    private ClientData clientData;

    @Rule
    public MockitoRule mockitoRule = MockitoJUnit.rule();

    @Test
    public void testCreate() {
        AccountCreation accountCreation = new AccountCreation();
        accountCreation.setAccountService(accountService);

        int clientId = accountCreation.createUser(clientData);
        assertNotEquals(0, clientId);
    }
}
