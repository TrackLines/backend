package es.tracklin.Client;

import es.tracklin.DataConnections.AccountService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;

import java.security.MessageDigest;
import java.util.Date;

@Component
public class AccountCreation {
    @Autowired
    AccountService accountService;

    public void setAccountService(AccountService accountService) {
        this.accountService = accountService;
    }

    public int createUser(ClientData clientData) {
        return accountService.createUser(clientData);
    }
}
